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

// The resend cooldown must still suppress the second email, but must NOT be
// observable in the response: a 429 here (and 200 for an unknown address) made
// the endpoint an account-existence oracle.
func TestForgotPassword_RateLimitedSilently(t *testing.T) {
	pool, r := setupHandlers(t)
	cap := installSender(t)
	_, _ = testutil.CreateUser(t, pool, "passenger", "rate-1")

	w := testutil.PostJSON(t, r, "/auth/forgot-password", "",
		map[string]any{"email": "rate-1@test.local"})
	require.Equal(t, http.StatusOK, w.Code)

	w = testutil.PostJSON(t, r, "/auth/forgot-password", "",
		map[string]any{"email": "rate-1@test.local"})
	assert.Equal(t, http.StatusOK, w.Code, "cooldown must not leak via status code")

	// Exactly one mail: the cooldown is still enforced, just silently.
	time.Sleep(300 * time.Millisecond)
	cap.mu.Lock()
	defer cap.mu.Unlock()
	assert.Equal(t, 1, cap.calls, "second request inside the cooldown must not send")
}

// The response to a known and an unknown address must be indistinguishable,
// including on the repeat request that trips the cooldown.
func TestForgotPassword_NoAccountEnumeration(t *testing.T) {
	pool, r := setupHandlers(t)
	installSender(t)
	_, _ = testutil.CreateUser(t, pool, "passenger", "enum-known")

	for i := 0; i < 2; i++ {
		known := testutil.PostJSON(t, r, "/auth/forgot-password", "",
			map[string]any{"email": "enum-known@test.local"})
		unknown := testutil.PostJSON(t, r, "/auth/forgot-password", "",
			map[string]any{"email": "enum-nobody@test.local"})
		assert.Equal(t, known.Code, unknown.Code,
			"status must not distinguish existing from missing accounts (pass %d)", i)
		assert.JSONEq(t, known.Body.String(), unknown.Body.String(),
			"body must not distinguish existing from missing accounts (pass %d)", i)
	}
}

// A fresh code must come with a fresh attempt budget. Without the attempts
// reset, burning the cap once locked the address out of password reset
// permanently — including for the legitimate owner.
func TestForgotPassword_NewCodeClearsAttempts(t *testing.T) {
	pool, r := setupHandlers(t)
	cap := installSender(t)
	_, _ = testutil.CreateUser(t, pool, "passenger", "lock-1")

	require.Equal(t, http.StatusOK, testutil.PostJSON(t, r, "/auth/forgot-password", "",
		map[string]any{"email": "lock-1@test.local"}).Code)

	// Burn the attempt cap with wrong codes (handlers.resetMaxAttempts == 5;
	// unexported, and this is an external test package).
	for i := 0; i < 5; i++ {
		testutil.PostJSON(t, r, "/auth/reset-password", "", map[string]any{
			"email": "lock-1@test.local", "code": "000000", "new_password": "newpass123",
		})
	}
	w := testutil.PostJSON(t, r, "/auth/reset-password", "", map[string]any{
		"email": "lock-1@test.local", "code": "000000", "new_password": "newpass123",
	})
	require.Equal(t, http.StatusTooManyRequests, w.Code, "cap should be reached")

	// Age the row past the resend cooldown, then request a fresh code.
	_, err := pool.Exec(context.Background(),
		`UPDATE password_resets SET created_at = NOW() - INTERVAL '5 minutes' WHERE email = $1`,
		"lock-1@test.local")
	require.NoError(t, err)

	cap.mu.Lock()
	cap.calls, cap.body = 0, ""
	cap.mu.Unlock()

	require.Equal(t, http.StatusOK, testutil.PostJSON(t, r, "/auth/forgot-password", "",
		map[string]any{"email": "lock-1@test.local"}).Code)

	deadline := time.Now().Add(2 * time.Second)
	var code string
	for time.Now().Before(deadline) {
		cap.mu.Lock()
		if cap.calls > 0 {
			code = extractCode(cap.body)
		}
		cap.mu.Unlock()
		if code != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.NotEmpty(t, code, "expected a fresh code to be mailed")

	// The new code must work — proving attempts was cleared, not carried over.
	w = testutil.PostJSON(t, r, "/auth/reset-password", "", map[string]any{
		"email": "lock-1@test.local", "code": code, "new_password": "newpass123",
	})
	assert.Equal(t, http.StatusOK, w.Code, "a fresh code must not inherit the old attempt count")
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

// Five codes per address per day: each code allows five guesses, so the
// cooldown alone left thousands of guesses a day against one account.
func TestForgotPassword_DailyCodeCap(t *testing.T) {
	pool, r := setupHandlers(t)
	cap := installSender(t)
	_, _ = testutil.CreateUser(t, pool, "passenger", "cap-1")
	const addr = "cap-1@test.local"

	request := func() {
		t.Helper()
		// Step past the 60 s resend cooldown.
		_, err := pool.Exec(ctx(t),
			`UPDATE password_resets SET created_at = created_at - INTERVAL '2 minutes' WHERE email = $1`, addr)
		require.NoError(t, err)
		w := testutil.PostJSON(t, r, "/auth/forgot-password", "", map[string]any{"email": addr})
		require.Equal(t, http.StatusOK, w.Code, "the cap must not leak via status code")
	}
	sent := func() int {
		time.Sleep(200 * time.Millisecond) // sends are async
		cap.mu.Lock()
		defer cap.mu.Unlock()
		return cap.calls
	}

	for i := 0; i < 6; i++ {
		request()
	}
	assert.Equal(t, 5, sent(), "sixth code within 24 h must not be sent")

	// A day later the window starts over.
	_, err := pool.Exec(ctx(t),
		`UPDATE password_resets SET window_started_at = NOW() - INTERVAL '25 hours' WHERE email = $1`, addr)
	require.NoError(t, err)
	request()
	assert.Equal(t, 6, sent())
}
