package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Pavelbotsu/sameway_backend/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func rateRouter(name string, max int, window time.Duration) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/x", middleware.RateLimit(name, max, window), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	return r
}

func hit(r *gin.Engine, ip string) int {
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = ip + ":12345"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code
}

func TestRateLimit_AllowsUpToMaxThenRejects(t *testing.T) {
	r := rateRouter("t-basic", 3, time.Minute)
	for i := 0; i < 3; i++ {
		assert.Equal(t, http.StatusOK, hit(r, "1.2.3.4"), "request %d should pass", i+1)
	}
	assert.Equal(t, http.StatusTooManyRequests, hit(r, "1.2.3.4"))
}

// One abusive caller must not spend everyone else's budget.
func TestRateLimit_IsPerCaller(t *testing.T) {
	r := rateRouter("t-percaller", 2, time.Minute)
	hit(r, "10.0.0.1")
	hit(r, "10.0.0.1")
	assert.Equal(t, http.StatusTooManyRequests, hit(r, "10.0.0.1"))
	assert.Equal(t, http.StatusOK, hit(r, "10.0.0.2"),
		"a different caller must have its own budget")
}

func TestRateLimit_WindowResets(t *testing.T) {
	r := rateRouter("t-window", 1, 60*time.Millisecond)
	assert.Equal(t, http.StatusOK, hit(r, "5.5.5.5"))
	assert.Equal(t, http.StatusTooManyRequests, hit(r, "5.5.5.5"))
	time.Sleep(80 * time.Millisecond)
	assert.Equal(t, http.StatusOK, hit(r, "5.5.5.5"), "budget should refresh after the window")
}

// Separate route names keep separate budgets, so a chatty endpoint can't
// exhaust the allowance of a sensitive one.
func TestRateLimit_IsPerRoute(t *testing.T) {
	a := rateRouter("t-route-a", 1, time.Minute)
	b := rateRouter("t-route-b", 1, time.Minute)
	assert.Equal(t, http.StatusOK, hit(a, "9.9.9.9"))
	assert.Equal(t, http.StatusTooManyRequests, hit(a, "9.9.9.9"))
	assert.Equal(t, http.StatusOK, hit(b, "9.9.9.9"))
}

func TestSweepRateLimits_DoesNotDropLiveWindows(t *testing.T) {
	r := rateRouter("t-sweep", 1, time.Minute)
	assert.Equal(t, http.StatusOK, hit(r, "8.8.8.8"))
	middleware.SweepRateLimits() // only evicts windows older than an hour
	assert.Equal(t, http.StatusTooManyRequests, hit(r, "8.8.8.8"),
		"sweep must not reset an in-flight window")
}
