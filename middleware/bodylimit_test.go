package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pavelbotsu/sameway_backend/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func bodyRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.BodyLimit(64, map[string]int64{"/big": 1024}))
	r.POST("/big", func(c *gin.Context) {
		var v map[string]any
		if err := c.ShouldBindJSON(&v); err != nil {
			c.Status(http.StatusBadRequest)
			return
		}
		c.Status(http.StatusOK)
	})
	r.POST("/x", func(c *gin.Context) {
		var v map[string]any
		if err := c.ShouldBindJSON(&v); err != nil {
			c.Status(http.StatusBadRequest)
			return
		}
		c.Status(http.StatusOK)
	})
	return r
}

func post(r *gin.Engine, body string) int {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w.Code
}

func TestBodyLimit(t *testing.T) {
	r := bodyRouter()
	assert.Equal(t, http.StatusOK, post(r, `{"a":"small"}`))
	assert.Equal(t, http.StatusBadRequest,
		post(r, `{"a":"`+strings.Repeat("x", 200)+`"}`), "over the cap must be rejected")
}

func TestBodyLimit_PerPathOverride(t *testing.T) {
	r := bodyRouter()
	body := `{"k":"` + strings.Repeat("x", 200) + `"}`
	assert.Equal(t, http.StatusBadRequest, post(r, body), "default path keeps the small cap")

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/big", strings.NewReader(body))
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code, "override path gets the larger cap")
}
