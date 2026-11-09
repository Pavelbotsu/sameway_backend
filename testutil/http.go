package testutil

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Pavelbotsu/sameway_backend/handlers"
	"github.com/Pavelbotsu/sameway_backend/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// NewTestRouter returns a Gin engine wired with the exact same route table as
// main.go. Uses gin.TestMode (no log spam). Hub is the package-level singleton
// in handlers — tests don't need to instantiate it.
func NewTestRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()

	auth := r.Group("/auth")
	{
		auth.POST("/register", handlers.Register)
		auth.POST("/login", handlers.Login)
		auth.POST("/forgot-password", handlers.ForgotPassword)
		auth.POST("/reset-password", handlers.ResetPassword)
		auth.GET("/profile", middleware.Auth(), handlers.GetProfile)
		auth.PUT("/profile", middleware.Auth(), handlers.UpdateProfile)
		auth.POST("/switch-role", middleware.Auth(), handlers.SwitchRole)
		auth.PUT("/fcm-token", middleware.Auth(), handlers.UpdateFCMToken)
		// Registered unconditionally here: tests run with APP_ENV unset (dev),
		// which is exactly when main.go registers them too.
		auth.GET("/fcm-status", middleware.Auth(), handlers.FCMStatus)
		auth.POST("/test-notification", middleware.Auth(), handlers.TestNotification)
		auth.GET("/stats", middleware.Auth(), handlers.GetStats)
		auth.POST("/change-password", middleware.Auth(), handlers.ChangePassword)
		auth.GET("/trips", middleware.Auth(), handlers.GetTripHistory)
		auth.POST("/bug-report", middleware.Auth(), handlers.SubmitBugReport)
		auth.POST("/test-ai", middleware.Auth(), handlers.TestAIRanker)
		auth.POST("/email-verify/request", middleware.Auth(), handlers.RequestEmailVerify)
		auth.POST("/email-verify/confirm", middleware.Auth(), handlers.ConfirmEmailVerify)
		auth.POST("/consent", middleware.Auth(), handlers.AcceptTerms)
		auth.POST("/logout", middleware.Auth(), handlers.Logout)
		auth.POST("/delete-account", middleware.Auth(), handlers.DeleteAccount)
	}

	r.POST("/ratings", middleware.Auth(), middleware.RequireConsent(), handlers.SubmitRating)
	r.POST("/pickup/confirm", middleware.Auth(), middleware.RequireConsent(), handlers.ConfirmPickup)
	r.GET("/pickup-route/:request_id", middleware.Auth(), middleware.RequireConsent(), handlers.GetPickupRoute)
	r.POST("/safety/flag", middleware.Auth(), handlers.FlagRideSafety)

	driver := r.Group("/driver", middleware.Auth(), middleware.RequireConsent(), middleware.RequireRole("driver"))
	{
		driver.POST("/route", handlers.SetRoute)
		driver.POST("/respond", handlers.DriverRespondToRide)
		driver.PUT("/location", handlers.UpdateLocation)
		driver.GET("/requests", handlers.GetRequests)
		driver.PUT("/status", handlers.UpdateStatus)
		driver.POST("/dropoff/:request_id", handlers.DropoffPassenger)
		driver.DELETE("/route", handlers.DeleteRoute)
		driver.DELETE("/ride/:request_id", handlers.DriverCancelRide)
		driver.POST("/trips", handlers.PlanTrip)
		driver.GET("/trips", handlers.GetMyPlannedTrips)
		driver.DELETE("/trips/:id", handlers.CancelPlannedTrip)
	}

	passenger := r.Group("/passenger", middleware.Auth(), middleware.RequireConsent(), middleware.RequireRole("passenger"))
	{
		passenger.PUT("/location", handlers.PassengerUpdateLocation)
		passenger.POST("/respond", handlers.RespondToRide)
		passenger.POST("/nearby-drivers", handlers.NearbyDrivers)
		passenger.POST("/search", handlers.PassengerSearch)
		passenger.POST("/request-driver", handlers.RequestDriver)
		passenger.POST("/reposition", handlers.RepositionForDriver)
		passenger.POST("/match-advice", handlers.MatchAdvice)
		passenger.GET("/my-requests", handlers.GetMyRequests)
		passenger.DELETE("/active", handlers.GoOffline)
		passenger.DELETE("/ride/:request_id", handlers.PassengerCancelRide)
		passenger.DELETE("/request/:request_id", handlers.PassengerCancelPendingRequest)
	}

	chat := r.Group("/chat", middleware.Auth(), middleware.RequireConsent())
	{
		chat.POST("/conversations", handlers.OpenConversation)
		chat.GET("/conversations", handlers.ListConversations)
		chat.GET("/conversations/:id/messages", handlers.GetConversationMessages)
		chat.POST("/send", handlers.SendMessage)
		chat.POST("/typing", handlers.TypingStatus)
		chat.POST("/read", handlers.MarkRead)
		chat.POST("/block", handlers.BlockUser)
		chat.DELETE("/block/:user_id", handlers.UnblockUser)
		chat.POST("/report", handlers.ReportUser)
		chat.GET("/:request_id", handlers.GetMessages)
	}

	r.POST("/trips/search", middleware.Auth(), middleware.RequireConsent(), handlers.SearchTrips)
	r.GET("/promotions", handlers.GetPromotions)
	r.GET("/legal/privacy", handlers.PrivacyPolicy)
	r.GET("/legal/terms", handlers.TermsOfService)
	r.GET("/account/delete", handlers.AccountDeletePage)
	r.POST("/account/delete", handlers.AccountDeleteWeb)
	r.GET("/ws", handlers.HandleWS)
	return r
}

// Do hits the engine with a single HTTP request, encoded as JSON if body is
// non-nil. Authorization header is set when token is non-empty.
func Do(t testing.TB, r *gin.Engine, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, reader)
	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// PostJSON is a thin convenience for the most common case.
func PostJSON(t testing.TB, r *gin.Engine, path, token string, body any) *httptest.ResponseRecorder {
	return Do(t, r, http.MethodPost, path, token, body)
}

func PutJSON(t testing.TB, r *gin.Engine, path, token string, body any) *httptest.ResponseRecorder {
	return Do(t, r, http.MethodPut, path, token, body)
}

func GetJSON(t testing.TB, r *gin.Engine, path, token string) *httptest.ResponseRecorder {
	return Do(t, r, http.MethodGet, path, token, nil)
}

func DeleteJSON(t testing.TB, r *gin.Engine, path, token string) *httptest.ResponseRecorder {
	return Do(t, r, http.MethodDelete, path, token, nil)
}

// DecodeJSON decodes the recorder body into v.
func DecodeJSON(t testing.TB, w *httptest.ResponseRecorder, v any) {
	t.Helper()
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), v), "decode response: %s", w.Body.String())
}
