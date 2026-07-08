package services

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"

	"github.com/Pavelbotsu/sameway_backend/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// pgQueryer is the subset of pgxpool.Pool / pgx.Tx that pickup-code writes
// need, so SetPickupCode can run inside the accept transaction.
type pgQueryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// generatePickupCode returns a 4-digit string (0000-9999) seeded from
// crypto/rand. Leading zeros are preserved.
func generatePickupCode() string {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		// rand.Read never errors on real Linux/macOS/Windows; if it does we
		// fall back to a sentinel that the confirmation flow will still
		// validate against.
		return "0000"
	}
	n := binary.BigEndian.Uint16(b[:]) % 10000
	return fmt.Sprintf("%04d", n)
}

// SetPickupCode generates a fresh code for the ride_request and writes it.
// Idempotent: if a code already exists on the row, returns the existing one
// instead of overwriting it.
func SetPickupCode(ctx context.Context, requestID string) (string, error) {
	return SetPickupCodeIn(ctx, db.DB, requestID)
}

// SetPickupCodeIn is SetPickupCode running on an arbitrary queryer — pass an
// open pgx.Tx to make the code write atomic with the accept (a ride must
// never commit as 'accepted' without a code, or ConfirmPickup can never
// succeed and the ride is stuck).
func SetPickupCodeIn(ctx context.Context, q pgQueryer, requestID string) (string, error) {
	var existing *string
	if err := q.QueryRow(ctx,
		`SELECT pickup_code FROM ride_requests WHERE id = $1`, requestID,
	).Scan(&existing); err != nil {
		return "", err
	}
	if existing != nil && *existing != "" {
		return *existing, nil
	}
	code := generatePickupCode()
	if _, err := q.Exec(ctx,
		`UPDATE ride_requests SET pickup_code = $1 WHERE id = $2`, code, requestID,
	); err != nil {
		return "", err
	}
	return code, nil
}
