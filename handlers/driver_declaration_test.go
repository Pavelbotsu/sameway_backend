package handlers_test

import (
	"net/http"
	"testing"

	"github.com/Pavelbotsu/sameway_backend/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var routeBody = map[string]any{
	"origin_lat": 50.0, "origin_lng": 20.0,
	"destination_lat": 50.0, "destination_lng": 20.1,
	"corridor_km": 1.0, "seats": 2,
}

// A driver must make the declaration and add car details before publishing
// a route (BlaBlaCar model: self-declared, documents not checked).
func TestSetRoute_NeedsDeclarationThenCarDetails(t *testing.T) {
	pool, r := setupHandlers(t)
	testutil.SwapRouter(t, &testutil.MockOSRM{})
	id, tok := testutil.CreateUser(t, pool, "driver", "decl-d")
	_, err := pool.Exec(ctx(t), `
		UPDATE users SET driver_declared_at = NULL, driver_declaration_version = NULL,
		       car_make = '', car_model = '', car_color = '', car_plate = ''
		WHERE id = $1`, id)
	require.NoError(t, err)

	w := testutil.PostJSON(t, r, "/driver/route", tok, routeBody)
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "driver_declaration_required")

	// Every statement must be confirmed.
	w = testutil.PostJSON(t, r, "/auth/driver-declaration", tok, map[string]any{
		"licence": true, "insurance": true, "roadworthy": true, "cost_sharing": false,
	})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	w = testutil.PostJSON(t, r, "/auth/driver-declaration", tok, map[string]any{
		"licence": true, "insurance": true, "roadworthy": true, "cost_sharing": true,
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	w = testutil.PostJSON(t, r, "/driver/route", tok, routeBody)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "car_details_required")

	w = testutil.PutJSON(t, r, "/auth/profile", tok, map[string]any{
		"car_make": "Skoda", "car_model": "Fabia", "car_color": "Red", "car_plate": "KR 12345",
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	w = testutil.PostJSON(t, r, "/driver/route", tok, routeBody)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var declared bool
	require.NoError(t, pool.QueryRow(ctx(t),
		`SELECT driver_declared_at IS NOT NULL FROM users WHERE id = $1`, id).Scan(&declared))
	assert.True(t, declared)
}

func TestPlanTrip_NeedsDeclaration(t *testing.T) {
	pool, r := setupHandlers(t)
	id, tok := testutil.CreateUser(t, pool, "driver", "decl-plan")
	_, err := pool.Exec(ctx(t), `UPDATE users SET driver_declaration_version = NULL WHERE id = $1`, id)
	require.NoError(t, err)
	w := testutil.PostJSON(t, r, "/driver/trips", tok, map[string]any{
		"origin_name": "A", "origin_lat": 50.0, "origin_lng": 20.0,
		"dest_name": "B", "dest_lat": 50.0, "dest_lng": 20.1,
		"seats": 2, "planned_at": "2030-01-01T08:00:00Z",
	})
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "driver_declaration_required")
}
