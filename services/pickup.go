package services

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"

	"github.com/Pavelbotsu/sameway_backend/db"
)

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
// instead of overwriting it. Used by both accept paths (driver-initiated and
// passenger-initiated) so the row carries the code regardless of which side
// flipped the status.
func SetPickupCode(ctx context.Context, requestID string) (string, error) {
	var existing *string
	if err := db.DB.QueryRow(ctx,
		`SELECT pickup_code FROM ride_requests WHERE id = $1`, requestID,
	).Scan(&existing); err != nil {
		return "", err
	}
	if existing != nil && *existing != "" {
		return *existing, nil
	}
	code := generatePickupCode()
	if _, err := db.DB.Exec(ctx,
		`UPDATE ride_requests SET pickup_code = $1 WHERE id = $2`, code, requestID,
	); err != nil {
		return "", err
	}
	return code, nil
}
