package main

import (
	"github.com/gin-gonic/gin"
)

func main() {
	// 1. Connect to Database
	dbPool := ConnectDB()
	defer dbPool.Close()

	// 2. Setup Router
	r := gin.Default()

	// Driver Routes
	r.POST("/driver/route", func(c *gin.Context) {
		SubmitDriverRoute(c, dbPool)
	})

	// Passenger Routes (Coming next)
	// r.PUT("/passenger/location", UpdatePassengerLocation)

	r.Run(":8080") // Listen on port 8080
}
