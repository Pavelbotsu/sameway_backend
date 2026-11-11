package handlers_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Pavelbotsu/sameway_backend/testutil"
	"github.com/stretchr/testify/assert"
)

func TestLegalPages_LanguageSelection(t *testing.T) {
	_, r := setupHandlers(t)
	get := func(path, acceptLang string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if acceptLang != "" {
			req.Header.Set("Accept-Language", acceptLang)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	cases := []struct{ path, accept, want string }{
		{"/legal/terms", "", "Terms of Service"},
		{"/legal/terms?lang=pl", "", "Regulamin"},
		{"/legal/terms?lang=uk", "en", "Умови користування"},
		{"/legal/privacy", "pl-PL,pl;q=0.9,en;q=0.8", "Polityka prywatności"},
		{"/legal/privacy", "uk-UA", "Політика конфіденційності"},
		{"/legal/privacy", "ru-RU", "Політика конфіденційності"},
		{"/legal/privacy", "de-DE", "Privacy Policy"},
		{"/legal/privacy?lang=xx", "", "Privacy Policy"},
	}
	for _, tc := range cases {
		w := get(tc.path, tc.accept)
		assert.Equal(t, http.StatusOK, w.Code, tc.path)
		assert.Contains(t, w.Body.String(), tc.want, "%s (Accept-Language %q)", tc.path, tc.accept)
	}

	css := testutil.GetJSON(t, r, "/legal/style.css", "")
	assert.Equal(t, http.StatusOK, css.Code)
	assert.Contains(t, css.Header().Get("Content-Type"), "text/css")
}

// Every language version must cover the same retention periods the job
// enforces (handlers/retention.go), and no unexpected placeholders.
func TestLegalPages_ConsistentWithRetention(t *testing.T) {
	_, r := setupHandlers(t)
	for _, lang := range []string{"en", "pl", "uk"} {
		body := testutil.GetJSON(t, r, "/legal/privacy?lang="+lang, "").Body.String()
		for _, want := range []string{"3 ", "12 ", "24 ", "14 ", "[OPERATOR NAME]", "[CONTACT EMAIL]", "[HOSTING PROVIDER]"} {
			assert.Contains(t, body, want, "privacy %s", lang)
		}
		assert.NotContains(t, body, "[RETENTION PERIOD]", lang)
	}
}
