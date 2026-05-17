package main

import (
	"log"

	"github.com/Pavelbotsu/rideshare-backend/config"
	"github.com/Pavelbotsu/rideshare-backend/db"
	"github.com/Pavelbotsu/rideshare-backend/handlers"
	"github.com/Pavelbotsu/rideshare-backend/middleware"
	"github.com/Pavelbotsu/rideshare-backend/services"
	"github.com/gin-gonic/gin"
)

func main() {
	config.Load()

	if err := db.Connect(); err != nil {
		log.Fatalf("postgres: %v", err)
	}
	defer db.DB.Close()

	if err := db.ConnectRedis(); err != nil {
		log.Printf("redis unavailable (%v) — continuing without it", err)
	}

	services.InitFirebase()

	r := gin.Default()

	auth := r.Group("/auth")
	{
		auth.POST("/register", handlers.Register)
		auth.POST("/login", handlers.Login)
		auth.PUT("/fcm-token", middleware.Auth(), handlers.UpdateFCMToken)
	}

	driver := r.Group("/driver", middleware.Auth(), middleware.RequireRole("driver"))
	{
		driver.POST("/route", handlers.SetRoute)
		driver.PUT("/location", handlers.UpdateLocation)
		driver.GET("/requests", handlers.GetRequests)
		driver.PUT("/status", handlers.UpdateStatus)
		driver.DELETE("/route", handlers.DeleteRoute)
	}

	passenger := r.Group("/passenger", middleware.Auth(), middleware.RequireRole("passenger"))
	{
		passenger.PUT("/location", handlers.PassengerUpdateLocation)
		passenger.POST("/respond", handlers.RespondToRide)
		passenger.DELETE("/active", handlers.GoOffline)
	}

	r.GET("/ws", handlers.HandleWS)

	log.Printf("Listening on :%s", config.C.Port)
	if err := r.Run(":" + config.C.Port); err != nil {
		log.Fatalf("server: %v", err)
	}
}
