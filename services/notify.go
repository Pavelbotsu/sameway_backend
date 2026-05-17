package services

import (
	"context"
	"log"

	firebase "firebase.google.com/go/v4"
	"firebase.google.com/go/v4/messaging"
	"github.com/Pavelbotsu/rideshare-backend/config"
	"google.golang.org/api/option"
)

var fcmClient *messaging.Client

func InitFirebase() {
	if config.C.FirebaseCredPath == "" {
		log.Println("Firebase credentials not set, FCM disabled")
		return
	}
	app, err := firebase.NewApp(context.Background(), nil, option.WithCredentialsFile(config.C.FirebaseCredPath))
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
		Data: map[string]string{
			"type":      "ride_request",
			"driver_id": driverID,
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
