/*
Copyright 2023. projectsveltos.io. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	goteamsnotify "github.com/atc0005/go-teams-notify/v2"
	"github.com/atc0005/go-teams-notify/v2/adaptivecard"
	"github.com/bwmarrin/discordgo"
	"github.com/go-logr/logr"
	webexteams "github.com/jbogarin/go-cisco-webex-teams/sdk"
	"github.com/slack-go/slack"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	appsv1alpha1 "gianlucam76/k8s-cleaner/api/v1alpha1"

	libsveltosv1alpha1 "github.com/projectsveltos/libsveltos/api/v1alpha1"
	libsveltosv1beta1 "github.com/projectsveltos/libsveltos/api/v1beta1"
	logs "github.com/projectsveltos/libsveltos/lib/logsettings"
	sveltosnotifications "github.com/projectsveltos/libsveltos/lib/notifications"
)

type slackInfo struct {
	token     string
	channelID string
}

type webexInfo struct {
	token string
	room  string
}

type discordInfo struct {
	token    string
	serverID string
}

type teamsInfo struct {
	webhookUrl string
}

// sendNotification delivers notification
func sendNotifications(ctx context.Context, resources []ResourceResult,
	cleaner *appsv1alpha1.Cleaner, logger logr.Logger) error {

	reportSpec := &appsv1alpha1.ReportSpec{}
	if len(cleaner.Spec.Notifications) > 0 {
		reportSpec = generateReportSpec(resources, cleaner)
	}

	message := fmt.Sprintf("This report has been generated by k8s-cleaner for instance: %s", cleaner.Name)

	for i := range cleaner.Spec.Notifications {
		notification := &cleaner.Spec.Notifications[i]
		logger = logger.WithValues("notification", fmt.Sprintf("%s:%s", notification.Type, notification.Name))
		logger.V(logs.LogDebug).Info("deliver notification")

		var err error

		// temporary conditional while implementing smtp notifications
		// type mismatch in the switch statement prevents this from being a case
		if string(notification.Type) == string(libsveltosv1beta1.NotificationTypeSMTP) {
			err = sendSmtpNotification(ctx, reportSpec, message, notification, logger)
		} else {
			switch notification.Type {
			case appsv1alpha1.NotificationTypeCleanerReport:
				err = createReportInstance(ctx, cleaner, reportSpec, logger)
			case appsv1alpha1.NotificationTypeSlack:
				err = sendSlackNotification(ctx, reportSpec, message, notification, logger)
			case appsv1alpha1.NotificationTypeWebex:
				err = sendWebexNotification(ctx, reportSpec, message, notification, logger)
			case appsv1alpha1.NotificationTypeDiscord:
				err = sendDiscordNotification(ctx, reportSpec, message, notification, logger)
			case appsv1alpha1.NotificationTypeTeams:
				err = sendTeamsNotification(ctx, reportSpec, message, notification, logger)
			default:
				logger.V(logs.LogInfo).Info("no handler registered for notification")
				panic(1)
			}
		}

		if err != nil {
			logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to send notification: %v", err))
			return err
		}
		logger.V(logs.LogDebug).Info("notification delivered")
	}
	return nil
}

func generateReportSpec(resources []ResourceResult, cleaner *appsv1alpha1.Cleaner) *appsv1alpha1.ReportSpec {
	reportSpec := appsv1alpha1.ReportSpec{}
	reportSpec.Action = cleaner.Spec.Action
	message := fmt.Sprintf(". time: %v", time.Now())

	reportSpec.ResourceInfo = make([]appsv1alpha1.ResourceInfo, len(resources))
	for i := range resources {
		reportSpec.ResourceInfo[i] = appsv1alpha1.ResourceInfo{
			Resource: corev1.ObjectReference{
				Namespace:  resources[i].Resource.GetNamespace(),
				Name:       resources[i].Resource.GetName(),
				Kind:       resources[i].Resource.GetKind(),
				APIVersion: resources[i].Resource.GetAPIVersion(),
			},
			Message: resources[i].Message + message,
		}
	}

	return &reportSpec
}

func createReportInstance(ctx context.Context, cleaner *appsv1alpha1.Cleaner,
	reportSpec *appsv1alpha1.ReportSpec, logger logr.Logger) error {

	report := &appsv1alpha1.Report{}
	err := k8sClient.Get(ctx, types.NamespacedName{Name: cleaner.Name}, report)
	if err != nil {
		if apierrors.IsNotFound(err) {
			logger.V(logs.LogInfo).Info("create report instance")
			report.Name = cleaner.Name
			report.Spec = *reportSpec
			return k8sClient.Create(ctx, report)
		}

		return err
	}

	report.Spec = *reportSpec
	logger.V(logs.LogInfo).Info("update report instance")
	return k8sClient.Update(ctx, report)
}

func sendSlackNotification(ctx context.Context, reportSpec *appsv1alpha1.ReportSpec,
	message string, notification *appsv1alpha1.Notification, logger logr.Logger) error {

	info, err := getSlackInfo(ctx, notification)
	if err != nil {
		return err
	}

	l := logger.WithValues("channel", info.channelID)
	l.V(logs.LogInfo).Info("send slack message")

	resourceSpecString, err := json.Marshal(*reportSpec)
	if err != nil {
		l.V(logs.LogInfo).Info(fmt.Sprintf("failed to marshal resourceSpec: %v", err))
		return err
	}

	attachment := slack.Attachment{
		Text: string(resourceSpecString),
	}

	api := slack.New(info.token)
	if api == nil {
		l.V(logs.LogInfo).Info("failed to get slack client")
	}

	_, _, err = api.PostMessage(info.channelID, slack.MsgOptionText(message, false), slack.MsgOptionAttachments(attachment))
	if err != nil {
		l.V(logs.LogInfo).Info(fmt.Sprintf("Failed to send message. Error: %v", err))
		return err
	}

	return nil
}

func sendTeamsNotification(ctx context.Context, reportSpec *appsv1alpha1.ReportSpec,
	message string, notification *appsv1alpha1.Notification, logger logr.Logger) error {

	info, err := getTeamsInfo(ctx, notification)
	if err != nil {
		return err
	}

	l := logger.WithValues("webhookUrl", info.webhookUrl)
	l.V(logs.LogInfo).Info("send teams message")

	teamsClient := goteamsnotify.NewTeamsClient()

	// Validate Teams Webhook expected format
	if teamsClient.ValidateWebhook(info.webhookUrl) != nil {
		l.V(logs.LogInfo).Info("failed to validate Teams webhook URL: %v", err)
		return err
	}

	resourceSpecData, err := json.Marshal(*reportSpec)
	if err != nil {
		l.V(logs.LogInfo).Info(fmt.Sprintf("failed to marshal resourceSpec: %v", err))
		return err
	}

	teamsMessage, err := adaptivecard.NewSimpleMessage(string(resourceSpecData), message, true)
	if err != nil {
		l.V(logs.LogInfo).Info("failed to create Teams message: %v", err)
		return err
	}

	// Send the meesage with the user provided webhook URL
	if teamsClient.Send(info.webhookUrl, teamsMessage) != nil {
		l.V(logs.LogInfo).Info("failed to send Teams message: %v", err)
		return err
	}

	return nil
}

func sendDiscordNotification(ctx context.Context, reportSpec *appsv1alpha1.ReportSpec,
	message string, notification *appsv1alpha1.Notification, logger logr.Logger) error {

	info, err := getDiscordInfo(ctx, notification)
	if err != nil {
		return err
	}

	l := logger.WithValues("room", info.serverID)
	l.V(logs.LogInfo).Info("send discord message")

	// Create a new Discord session using the provided token
	dg, err := discordgo.New("Bot " + info.token)
	if err != nil {
		l.V(logs.LogInfo).Info("failed to get discord session")
		return err
	}

	resourceSpecData, err := json.Marshal(*reportSpec)
	if err != nil {
		l.V(logs.LogInfo).Info(fmt.Sprintf("failed to marshal resourceSpec: %v", err))
		return err
	}

	// Create a temporary file
	tmpFile, err := os.CreateTemp(os.TempDir(), "k8s-cleaner-webex")
	if err != nil {
		l.V(logs.LogInfo).Info(fmt.Sprintf("error creating temporary file: %v", err))
		return err
	}

	defer func() {
		// Close the file
		tmpFile.Close()

		// Remove the temporary file
		os.Remove(tmpFile.Name())
	}()

	_, err = tmpFile.Write(resourceSpecData)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to write to file: %s", err))
		return err
	}

	// Open the temporary file for reading
	withFileReader := func() (io.Reader, error) {
		var fileContentReader *os.File
		fileContentReader, err = os.Open(tmpFile.Name())
		if err != nil {
			return nil, fmt.Errorf("error opening file: %w", err)
		}

		return fileContentReader, nil
	}

	// Create the attachment object
	fileReader, err := withFileReader()
	if err != nil {
		return err
	}

	// Create a new message with both a text content and the file attachment
	_, err = dg.ChannelMessageSendComplex(info.serverID, &discordgo.MessageSend{
		Content: message,
		Files: []*discordgo.File{
			{
				Name:   "k8s-cleaner-report", // Replace with desired filename
				Reader: fileReader,
			},
		},
	})

	return err
}

func sendSmtpNotification(ctx context.Context, reportSpec *appsv1alpha1.ReportSpec,
	message string, notification *appsv1alpha1.Notification, logger logr.Logger) error {
	sveltosNotification := &libsveltosv1beta1.Notification{
		Name:            notification.Name,
		Type:            libsveltosv1beta1.NotificationTypeSMTP,
		NotificationRef: notification.NotificationRef,
	}

	mailer, err := sveltosnotifications.NewMailer(ctx, k8sClient, sveltosNotification)
	if err != nil {
		return err
	}

	l := logger.WithValues("notification", fmt.Sprintf("%s:%s", notification.Type, notification.Name))
	l.V(logs.LogInfo).Info("send smtp message")

	resourceSpecData, err := json.Marshal(*reportSpec)
	if err != nil {
		l.V(logs.LogInfo).Info(fmt.Sprintf("failed to marshal resourceSpec: %v", err))
	}
	return mailer.SendMail(message, string(resourceSpecData), false)
}

func sendWebexNotification(ctx context.Context, reportSpec *appsv1alpha1.ReportSpec,
	message string, notification *appsv1alpha1.Notification, logger logr.Logger) error {

	info, err := getWebexInfo(ctx, notification)
	if err != nil {
		return err
	}

	l := logger.WithValues("room", info.room)
	l.V(logs.LogInfo).Info("send webex message")

	webexClient := webexteams.NewClient()
	if webexClient == nil {
		l.V(logs.LogInfo).Info("failed to get webexClient client")
		return fmt.Errorf("failed to get webexClient client")
	}
	webexClient.SetAuthToken(info.token)

	webexMessage := &webexteams.MessageCreateRequest{
		Markdown: message,
		RoomID:   info.room,
	}

	resourceSpecData, err := json.Marshal(*reportSpec)
	if err != nil {
		l.V(logs.LogInfo).Info(fmt.Sprintf("failed to marshal resourceSpec: %v", err))
		return err
	}

	// Create a temporary file
	tmpFile, err := os.CreateTemp(os.TempDir(), "k8s-cleaner-webex")
	if err != nil {
		l.V(logs.LogInfo).Info(fmt.Sprintf("error creating temporary file: %v", err))
		return err
	}

	defer func() {
		// Close the file
		tmpFile.Close()

		// Remove the temporary file
		os.Remove(tmpFile.Name())
	}()

	_, err = tmpFile.Write(resourceSpecData)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to write to file: %s", err))
		return err
	}

	// Open the temporary file for reading
	withFileReader := func() (io.Reader, error) {
		var fileContentReader *os.File
		fileContentReader, err = os.Open(tmpFile.Name())
		if err != nil {
			return nil, fmt.Errorf("Error opening file: %w", err)
		}

		return fileContentReader, nil
	}

	// Create the attachment object
	fileReader, err := withFileReader()
	if err != nil {
		return err
	}

	webexFile := webexteams.File{
		Name:        tmpFile.Name(),
		Reader:      fileReader,
		ContentType: "multipart/form-data",
	}

	webexMessage.Files = []webexteams.File{webexFile}

	_, resp, err := webexClient.Messages.CreateMessage(webexMessage)
	if err != nil {
		l.V(logs.LogInfo).Info(fmt.Sprintf("Failed to send message. Error: %v", err))
		return err
	}

	if resp != nil {
		l.V(logs.LogDebug).Info(fmt.Sprintf("response: %s", string(resp.Body())))
	}

	return nil
}

func getSlackInfo(ctx context.Context, notification *appsv1alpha1.Notification) (*slackInfo, error) {
	secret, err := getSecret(ctx, notification)
	if err != nil {
		return nil, err
	}

	authToken, ok := secret.Data[libsveltosv1alpha1.SlackToken]
	if !ok {
		return nil, fmt.Errorf("secret does not contain slack token")
	}

	channelID, ok := secret.Data[libsveltosv1alpha1.SlackChannelID]
	if !ok {
		return nil, fmt.Errorf("secret does not contain slack channelID")
	}

	return &slackInfo{token: string(authToken), channelID: string(channelID)}, nil
}

func getTeamsInfo(ctx context.Context, notification *appsv1alpha1.Notification) (*teamsInfo, error) {
	secret, err := getSecret(ctx, notification)
	if err != nil {
		return nil, err
	}

	webhookUrl, ok := secret.Data[libsveltosv1alpha1.TeamsWebhookURL]
	if !ok {
		return nil, fmt.Errorf("secret does not contain webhook URL")
	}

	return &teamsInfo{webhookUrl: string(webhookUrl)}, nil
}

func getDiscordInfo(ctx context.Context, notification *appsv1alpha1.Notification) (*discordInfo, error) {
	secret, err := getSecret(ctx, notification)
	if err != nil {
		return nil, err
	}

	authToken, ok := secret.Data[libsveltosv1alpha1.DiscordToken]
	if !ok {
		return nil, fmt.Errorf("secret does not contain discord token")
	}

	serverID, ok := secret.Data[libsveltosv1alpha1.DiscordChannelID]
	if !ok {
		return nil, fmt.Errorf("secret does not contain discord channel id")
	}

	return &discordInfo{token: string(authToken), serverID: string(serverID)}, nil
}

func getWebexInfo(ctx context.Context, notification *appsv1alpha1.Notification) (*webexInfo, error) {
	secret, err := getSecret(ctx, notification)
	if err != nil {
		return nil, err
	}

	authToken, ok := secret.Data[libsveltosv1alpha1.WebexToken]
	if !ok {
		return nil, fmt.Errorf("secret does not contain webex token")
	}

	room, ok := secret.Data[libsveltosv1alpha1.WebexRoomID]
	if !ok {
		return nil, fmt.Errorf("secret does not contain webex room")
	}

	return &webexInfo{token: string(authToken), room: string(room)}, nil
}

func getSecret(ctx context.Context, notification *appsv1alpha1.Notification) (*corev1.Secret, error) {
	if notification.NotificationRef == nil {
		return nil, fmt.Errorf("notification must reference secret containing slack token/channel id")
	}

	if notification.NotificationRef.Kind != "Secret" {
		return nil, fmt.Errorf("notification must reference secret containing slack token/channel id")
	}

	if notification.NotificationRef.APIVersion != "v1" {
		return nil, fmt.Errorf("notification must reference secret containing slack token/channel id")
	}

	secret := &corev1.Secret{}
	err := k8sClient.Get(ctx, types.NamespacedName{
		Namespace: notification.NotificationRef.Namespace,
		Name:      notification.NotificationRef.Name,
	}, secret)
	if err != nil {
		return nil, err
	}

	if secret.Data == nil {
		return nil, fmt.Errorf("notification must reference secret containing slack token/channel id")
	}

	return secret, nil
}
