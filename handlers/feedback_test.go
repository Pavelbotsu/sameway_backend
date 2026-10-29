package handlers_test

import (
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Pavelbotsu/sameway_backend/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSubmitBugReport_HappyPath(t *testing.T) {
	pool, r := setupHandlers(t)
	userID, tok := testutil.CreateUser(t, pool, "passenger", "bug-reporter")

	w := testutil.PostJSON(t, r, "/auth/bug-report", tok, map[string]any{
		"title":       "App crashes when I tap the map",
		"body":        "Steps to reproduce: open map, double-tap, app freezes.",
		"app_version": "1.0.3",
		"device_info": "Pixel 6 / Android 14",
	})
	require.Equal(t, http.StatusOK, w.Code)

	var title, body, appV, dev string
	var storedUserID string
	require.NoError(t, pool.QueryRow(ctx(t), `
		SELECT user_id, title, body, COALESCE(app_version,''), COALESCE(device_info,'')
		FROM bug_reports WHERE user_id = $1
		ORDER BY created_at DESC LIMIT 1
	`, userID).Scan(&storedUserID, &title, &body, &appV, &dev))
	assert.Equal(t, userID, storedUserID)
	assert.Contains(t, title, "crashes")
	assert.Contains(t, body, "Steps")
	assert.Equal(t, "1.0.3", appV)
	assert.Equal(t, "Pixel 6 / Android 14", dev)
}

func TestSubmitBugReport_RequiresAuth(t *testing.T) {
	_, r := setupHandlers(t)
	w := testutil.PostJSON(t, r, "/auth/bug-report", "", map[string]any{
		"title": "x", "body": "y",
	})
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestSubmitBugReport_RejectsEmptyFields(t *testing.T) {
	pool, r := setupHandlers(t)
	_, tok := testutil.CreateUser(t, pool, "passenger", "bug-empty")

	w := testutil.PostJSON(t, r, "/auth/bug-report", tok, map[string]any{
		"title": "", "body": "ok",
	})
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// Byte-slicing a Cyrillic string at the length cap lands mid-character and
// produces invalid UTF-8, which Postgres rejects — a 500 on a long bug report.
// The app is fully localized into Ukrainian, so this is the common path.
func TestSubmitBugReport_TruncatesMultiByteSafely(t *testing.T) {
	pool, r := setupHandlers(t)
	_, tok := testutil.CreateUser(t, pool, "passenger", "bug-utf8")

	// 1 ASCII + Cyrillic: the 200-BYTE boundary now falls inside a 2-byte
	// character, which is exactly the case that used to blow up.
	title := "x" + strings.Repeat("д", 300)
	body := strings.Repeat("привіт ", 900) // comfortably over the 4000 cap

	w := testutil.PostJSON(t, r, "/auth/bug-report", tok, map[string]any{
		"title": title, "body": body,
	})
	require.Equal(t, http.StatusOK, w.Code, "multi-byte input must not 500")

	var gotTitle, gotBody string
	require.NoError(t, pool.QueryRow(ctx(t),
		`SELECT title, body FROM bug_reports ORDER BY created_at DESC LIMIT 1`,
	).Scan(&gotTitle, &gotBody))

	assert.True(t, utf8.ValidString(gotTitle), "stored title must be valid UTF-8")
	assert.True(t, utf8.ValidString(gotBody), "stored body must be valid UTF-8")
	assert.Equal(t, 200, utf8.RuneCountInString(gotTitle), "cap is in characters")
	assert.Equal(t, 4000, utf8.RuneCountInString(gotBody))
}
