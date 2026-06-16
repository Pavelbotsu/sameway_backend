package handlers_test

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pavelbotsu/sameway_backend/services/email"
	"github.com/Pavelbotsu/sameway_backend/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

// captureSender records the last Send call so tests can grab the generated
// code without standing up SMTP. Installed as email.Active before each test
// and restored on cleanup.
type captureSender struct {
	mu    sync.Mutex
	to    string
	body  string
	calls int
}

func (s *captureSender) Send(_ context.Context, to, _, body string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.to = to
	s.body = body
	s.calls++
	return nil
}

func installSender(t *testing.T) *captureSender {
	t.Helper()
	cap := &captureSender{}
	prev := email.Active
	email.Active = cap
	t.Cleanup(func() { email.Active = prev })
	return cap
}

// extractCode pulls the 6-digit code out of "...code is 482917. ..." body.
func extractCode(body string) string {
	const prefix = "code is "
	i := strings.Index(body, prefix)
	if i < 0 {
		return ""
	}
	rest := body[i+len(prefix):]
	if len(rest) < 6 {
		return ""
	}
	return rest[:6]
}

func TestForgotPassword_HappyPath_EmailsCode(t *testing.T) {
	pool, r := setupHandlers(t)
	cap := installSender(t)
	_, _ = testutil.CreateUser(t, pool, "passenger", "pwd-1")

	w := testutil.PostJSON(t, r, "/auth/forgot-password", "",
		map[string]any{"email": "pwd-1@test.local"})
	require.Equal(t, http.StatusOK, w.Code)

	// Give the goroutine a beat to fire.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		cap.mu.Lock()
		calls := cap.calls
		cap.mu.Unlock()
		if calls > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cap.mu.Lock()
	assert.Equal(t, 1, cap.calls, "expected exactly one send")
	assert.Equal(t, "pwd-1@test.local", cap.to)
	body := cap.body
	cap.mu.Unlock()
	assert.NotEmpty(t, extractCode(body))
}

func TestForgotPassword_UnknownEmail_Returns200_NoSend(t *testing.T) {
	_, r := setupHandlers(t)
	cap := installSender(t)

	w := testutil.PostJSON(t, r, "/auth/forgot-password", "",
		map[string]any{"email": "not-a-user@nowhere.test"})
	require.Equal(t, http.StatusOK, w.Code, "must not leak account existence")

	time.Sleep(200 * time.Millisecond)
	cap.mu.Lock()
	assert.Equal(t, 0, cap.calls, "must not send mail to unknown addresses")
	cap.mu.Unlock()
}

func TestForgotPassword_RateLimited(t *testing.T) {
	pool, r := setupHandlers(t)
	installSender(t)
	_, _ = testutil.CreateUser(t, pool, "passenger", "rate-1")

	w := testutil.PostJSON(t, r, "/auth/forgot-password", "",
		map[string]any{"email": "rate-1@test.local"})
	require.Equal(t, http.StatusOK, w.Code)

	w = testutil.PostJSON(t, r, "/auth/forgot-password", "",
		map[string]any{"email": "rate-1@test.local"})
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
}

func TestResetPassword_HappyPath(t *testing.T) {
	pool, r := setupHandlers(t)
	cap := installSender(t)
	_, _ = testutil.CreateUser(t, pool, "passenger", "rst-1")

	w := testutil.PostJSON(t, r, "/auth/forgot-password", "",
		map[string]any{"email": "rst-1@test.local"})
	require.Equal(t, http.StatusOK, w.Code)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		cap.mu.Lock()
		calls := cap.calls
		cap.mu.Unlock()
		if calls > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cap.mu.Lock()
	code := extractCode(cap.body)
	cap.mu.Unlock()
	require.Len(t, code, 6, "must have a 6-digit code in the mail body")

	w = testutil.PostJSON(t, r, "/auth/reset-password", "", map[string]any{
		"email":        "rst-1@test.local",
		"code":         code,
		"new_password": "br4nd-n3w-passw0rd",
	})
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"token":`,
		"reset must return a JWT so the client auto-signs-in")

	// Bcrypt verify against the stored hash.
	var hash string
	require.NoError(t, pool.QueryRow(ctx(t),
		`SELECT password FROM users WHERE email = $1`,
		"rst-1@test.local").Scan(&hash))
	assert.NoError(t, bcrypt.CompareHashAndPassword([]byte(hash), []byte("br4nd-n3w-passw0rd")))

	// Reset row must be deleted (one-shot).
	var exists bool
	require.NoError(t, pool.QueryRow(ctx(t),
		`SELECT EXISTS(SELECT 1 FROM password_resets WHERE email = $1)`,
		"rst-1@test.local").Scan(&exists))
	assert.False(t, exists, "row must be consumed on successful reset")
}

func TestResetPassword_WrongCode(t *testing.T) {
	pool, r := setupHandlers(t)
	installSender(t)
	_, _ = testutil.CreateUser(t, pool, "passenger", "rst-bad")

	w := testutil.PostJSON(t, r, "/auth/forgot-password", "",
		map[string]any{"email": "rst-bad@test.local"})
	require.Equal(t, http.StatusOK, w.Code)

	w = testutil.PostJSON(t, r, "/auth/reset-password", "", map[string]any{
		"email":        "rst-bad@test.local",
		"code":         "000000",
		"new_password": "anything-ok-here",
	})
	assert.Equal(t, http.StatusBadRequest, w.Code)

	// Reset row must still exist — wrong code is not a consumption event.
	var exists bool
	require.NoError(t, pool.QueryRow(ctx(t),
		`SELECT EXISTS(SELECT 1 FROM password_resets WHERE email = $1)`,
		"rst-bad@test.local").Scan(&exists))
	assert.True(t, exists, "wrong-code attempts must not consume the reset row")
}

func TestResetPassword_ShortPassword(t *testing.T) {
	pool, r := setupHandlers(t)
	installSender(t)
	_, _ = testutil.CreateUser(t, pool, "passenger", "rst-short")

	w := testutil.PostJSON(t, r, "/auth/forgot-password", "",
		map[string]any{"email": "rst-short@test.local"})
	require.Equal(t, http.StatusOK, w.Code)

	w = testutil.PostJSON(t, r, "/auth/reset-password", "", map[string]any{
		"email":        "rst-short@test.local",
		"code":         "123456",
		"new_password": "12345",
	})
	assert.Equal(t, http.StatusBadRequest, w.Code)
}
