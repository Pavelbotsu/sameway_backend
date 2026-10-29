package middleware

import (
	"log"
	"time"

	"github.com/gin-gonic/gin"
)

// Logger is a request logger that deliberately does NOT log the query string.
//
// gin.Default()'s logger records `path + "?" + rawQuery`. The WebSocket
// handshake carries the JWT as `/ws?token=<jwt>` (browsers cannot set headers
// on a WS upgrade), so the stock logger writes a 30-day session token into the
// container log on every connect — and from there into whatever ships those
// logs. Logging only the routed path keeps that out of persistent storage.
//
// Status, latency, client IP and method are still recorded; only the raw query
// is dropped.
func Logger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path // never c.Request.URL.RequestURI()

		c.Next()

		log.Printf("[GIN] %3d | %13v | %15s | %-7s %s",
			c.Writer.Status(),
			time.Since(start),
			c.ClientIP(),
			c.Request.Method,
			path,
		)
	}
}
