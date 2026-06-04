package main

import (
	"log"

	"github.com/Pavelbotsu/sameway_backend/config"
	"github.com/Pavelbotsu/sameway_backend/db"
	"github.com/Pavelbotsu/sameway_backend/handlers"
	"github.com/Pavelbotsu/sameway_backend/middleware"
	"github.com/Pavelbotsu/sameway_backend/services"
	"github.com/gin-gonic/gin"
)

func main() {
	config.Load()

	if err := db.Connect(); err != nil {
		log.Fatalf("postgres: %v", err)
	}
	defer db.DB.Close()

	if err := db.ConnectRedis(); err != nil {
		log.Printf("redis unavailable (%v) â€” continuing without it", err)
	}

	services.InitFirebase()

	r := gin.Default()

	auth := r.Group("/auth")
	{
		auth.POST("/register", handlers.Register)
		auth.POST("/login", handlers.Login)
		auth.GET("/profile", middleware.Auth(), handlers.GetProfile)
		auth.PUT("/profile", middleware.Auth(), handlers.UpdateProfile)
		auth.POST("/switch-role", middleware.Auth(), handlers.SwitchRole)
		auth.PUT("/fcm-token", middleware.Auth(), handlers.UpdateFCMToken)
		auth.GET("/fcm-status", middleware.Auth(), handlers.FCMStatus)
		auth.POST("/test-notification", middleware.Auth(), handlers.TestNotification)
		auth.GET("/stats", middleware.Auth(), handlers.GetStats)
		auth.POST("/change-password", middleware.Auth(), handlers.ChangePassword)
		auth.GET("/trips", middleware.Auth(), handlers.GetTripHistory)
	}

	r.POST("/ratings", middleware.Auth(), handlers.SubmitRating)

	driver := r.Group("/driver", middleware.Auth(), middleware.RequireRole("driver"))
	{
		driver.POST("/route", handlers.SetRoute)
		driver.POST("/respond", handlers.DriverRespondToRide)
		driver.PUT("/location", handlers.UpdateLocation)
		driver.GET("/requests", handlers.GetRequests)
		driver.PUT("/status", handlers.UpdateStatus)
		driver.DELETE("/route", handlers.DeleteRoute)
		driver.DELETE("/ride/:request_id", handlers.DriverCancelRide)
		driver.POST("/trips", handlers.PlanTrip)
		driver.GET("/trips", handlers.GetMyPlannedTrips)
		driver.DELETE("/trips/:id", handlers.CancelPlannedTrip)
	}

	passenger := r.Group("/passenger", middleware.Auth(), middleware.RequireRole("passenger"))
	{
		passenger.PUT("/location", handlers.PassengerUpdateLocation)
		passenger.POST("/respond", handlers.RespondToRide)
		passenger.POST("/nearby-drivers", handlers.NearbyDrivers)
		passenger.POST("/request-driver", handlers.RequestDriver)
		passenger.GET("/my-requests", handlers.GetMyRequests)
		passenger.DELETE("/active", handlers.GoOffline)
		passenger.DELETE("/ride/:request_id", handlers.PassengerCancelRide)
		passenger.DELETE("/request/:request_id", handlers.PassengerCancelPendingRequest)
	}

	chat := r.Group("/chat", middleware.Auth())
	{
		chat.POST("/send", handlers.SendMessage)
		chat.GET("/:request_id", handlers.GetMessages)
	}

	r.POST("/trips/search", handlers.SearchTrips)
	r.GET("/promotions", handlers.GetPromotions)

	r.GET("/ws", handlers.HandleWS)

	go handlers.ReapPendingRideRequests(config.C.RideRequestTimeoutMinutes)

	log.Printf("Listening on :%s", config.C.Port)
	if err := r.Run(":" + config.C.Port); err != nil {
		log.Fatalf("server: %v", err)
	}
}
