package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// BodyLimit caps request bodies at maxBytes. Reading past the cap fails, so
// ShouldBindJSON returns an error and the handler answers 400 — the server
// never buffers an arbitrarily large upload. Gin itself sets no limit.
//
// overrides gives specific paths (exact URL path, e.g. the photo upload) a
// different cap. It has to be decided here: a second MaxBytesReader further
// down can only lower the limit, never raise it.
func BodyLimit(maxBytes int64, overrides map[string]int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Body != nil {
			limit := maxBytes
			if n, ok := overrides[c.Request.URL.Path]; ok {
				limit = n
			}
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
		}
		c.Next()
	}
}
