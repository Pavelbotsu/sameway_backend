package services_test

import (
	"testing"

	"github.com/Pavelbotsu/sameway_backend/services"
)

// FCM is intentionally not initialized in any test. These helpers must remain
// safe to call when fcmClient is nil — otherwise the production handler path
// (which calls them whether FCM is wired or not) would crash.

func TestNotify_NoPanicWhenDisabled(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("notify helpers panicked: %v", r)
		}
	}()

	// These accept an FCM token and ignore everything when fcmClient is nil.
	services.NotifyPassengerRideAvailable("", "driver-id", 50.0, 20.0)
	services.NotifyDriverPassengerResponded("", "passenger-id", "accepted")
	services.NotifyDriverPassengerRequested("", "passenger-id")
}

func TestIsFCMReady_FalseByDefault(t *testing.T) {
	if services.IsFCMReady() {
		t.Fatal("FCM should not be ready in the test environment")
	}
}
