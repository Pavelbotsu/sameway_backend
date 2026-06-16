package services

import (
	"context"
	"fmt"
	"log"

	firebase "firebase.google.com/go/v4"
	"firebase.google.com/go/v4/messaging"
	"github.com/Pavelbotsu/sameway_backend/config"
	"google.golang.org/api/option"
)

var fcmClient *messaging.Client

// Three named channels matching the Flutter client's FcmChannels definitions.
// Android routes the notification to the matching channel so the user can
// silence chat without losing critical ride alerts.
const (
	channelRide      = "ride_critical"
	channelChat      = "chat"
	channelMarketing = "marketing"
)

func androidConfig(channelID string) *messaging.AndroidConfig {
	return &messaging.AndroidConfig{
		Notification: &messaging.AndroidNotification{
			ChannelID: channelID,
		},
	}
}

func InitFirebase() {
	if config.C.FirebaseCredPath == "" {
		log.Println("Firebase credentials not set, FCM disabled")
		return
	}
	app, err := firebase.NewApp(context.Background(), nil, option.WithAuthCredentialsFile(option.ServiceAccount, config.C.FirebaseCredPath))
	if err != nil {
		log.Printf("firebase init error: %v", err)
		return
	}
	client, err := app.Messaging(context.Background())
	if err != nil {
		log.Printf("firebase messaging init error: %v", err)
		return
	}
	fcmClient = client
}

func NotifyPassengerRideAvailable(fcmToken, driverID string, destLat, destLng float64) {
	if fcmClient == nil || fcmToken == "" {
		return
	}
	msg := &messaging.Message{
		Token: fcmToken,
		Notification: &messaging.Notification{
			Title: "Ride available!",
			Body:  "A driver heading your way — tap to respond",
		},
		Android: androidConfig(channelRide),
		Data: map[string]string{
			"type":      "ride_request",
			"driver_id": driverID,
		},
	}
	if _, err := fcmClient.Send(context.Background(), msg); err != nil {
		log.Printf("fcm send error: %v", err)
	}
}

func IsFCMReady() bool { return fcmClient != nil }

func SendTestNotification(fcmToken string) error {
	if fcmClient == nil {
		return fmt.Errorf("FCM not initialized — check FIREBASE_CREDENTIALS_PATH")
	}
	if fcmToken == "" {
		return fmt.Errorf("no FCM token registered for this device yet")
	}
	msg := &messaging.Message{
		Token: fcmToken,
		Notification: &messaging.Notification{
			Title: "Firebase connected!",
			Body:  "Sameway backend successfully reached your device.",
		},
		Android: androidConfig(channelMarketing),
		Data:    map[string]string{"type": "test"},
	}
	if _, err := fcmClient.Send(context.Background(), msg); err != nil {
		return fmt.Errorf("fcm send: %w", err)
	}
	return nil
}

func NotifyDriverPassengerRequested(fcmToken, passengerID string) {
	if fcmClient == nil || fcmToken == "" {
		return
	}
	msg := &messaging.Message{
		Token: fcmToken,
		Notification: &messaging.Notification{
			Title: "Ride request!",
			Body:  "A passenger wants to join your route — tap to respond",
		},
		Android: androidConfig(channelRide),
		Data: map[string]string{
			"type":         "ride_request",
			"passenger_id": passengerID,
		},
	}
	if _, err := fcmClient.Send(context.Background(), msg); err != nil {
		log.Printf("fcm send error: %v", err)
	}
}

// NotifyChatMessage delivers a backgrounded-app push for a new chat message
// so the recipient sees "<sender>: <preview>" outside the foreground WS path.
// The body is truncated to 140 chars to fit a single Android/iOS line.
func NotifyChatMessage(fcmToken, senderID, senderName, rideRequestID, content string) {
	if fcmClient == nil || fcmToken == "" {
		return
	}
	preview := content
	if len([]rune(preview)) > 140 {
		runes := []rune(preview)
		preview = string(runes[:140]) + "…"
	}
	title := senderName
	if title == "" {
		title = "New message"
	}
	msg := &messaging.Message{
		Token: fcmToken,
		Notification: &messaging.Notification{
			Title: title,
			Body:  preview,
		},
		Android: androidConfig(channelChat),
		Data: map[string]string{
			"type":            "chat_message",
			"sender_id":       senderID,
			"ride_request_id": rideRequestID,
		},
	}
	if _, err := fcmClient.Send(context.Background(), msg); err != nil {
		log.Printf("fcm send error: %v", err)
	}
}

func NotifyDriverPassengerResponded(fcmToken, passengerID, status string) {
	if fcmClient == nil || fcmToken == "" {
		return
	}
	body := "A passenger accepted your ride!"
	if status == "declined" {
		body = "A passenger declined your ride."
	}
	msg := &messaging.Message{
		Token: fcmToken,
		Notification: &messaging.Notification{
			Title: "Ride response",
			Body:  body,
		},
		Android: androidConfig(channelRide),
		Data: map[string]string{
			"type":         "ride_response",
			"passenger_id": passengerID,
			"status":       status,
		},
	}
	if _, err := fcmClient.Send(context.Background(), msg); err != nil {
		log.Printf("fcm send error: %v", err)
	}
}
