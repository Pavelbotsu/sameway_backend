package handlers_test

import (
	"net/http"
	"testing"

	"github.com/Pavelbotsu/sameway_backend/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExportData_ContainsTheUsersData(t *testing.T) {
	pool, r := setupHandlers(t)
	paxID, paxTok, driverID, _, doneID := seedDeletionFixture(t, pool)
	_ = doneID

	w := testutil.GetJSON(t, r, "/auth/export", paxTok)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Header().Get("Content-Disposition"), "attachment")

	var out map[string]any
	testutil.DecodeJSON(t, w, &out)
	profile := out["profile"].(map[string]any)
	assert.Equal(t, paxID, profile["id"])
	assert.Equal(t, "pax@test.local", profile["email"])
	assert.NotContains(t, profile, "password", "never export the password hash")

	rides := out["rides"].([]any)
	assert.Len(t, rides, 2) // the done ride + the open offer
	first := rides[0].(map[string]any)
	assert.Equal(t, "passenger", first["your_role"])
	assert.Equal(t, driverID, first["other_user_id"])

	assert.Len(t, out["messages_sent"].([]any), 1)
	assert.Len(t, out["ratings_given"].([]any), 1)
	assert.Len(t, out["ratings_received"].([]any), 1)
	assert.Len(t, out["bug_reports"].([]any), 1)
}

func TestExportData_AllowedWhileSuspended(t *testing.T) {
	pool, r := setupHandlers(t)
	id, tok := testutil.CreateUser(t, pool, "passenger", "p1")
	suspend(t, id)
	assert.Equal(t, http.StatusOK, testutil.GetJSON(t, r, "/auth/export", tok).Code)
}
