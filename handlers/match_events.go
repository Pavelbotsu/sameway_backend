package handlers

import (
	"context"
	"encoding/json"
	"log"

	"github.com/Pavelbotsu/sameway_backend/db"
)

// Outcome logging for the Round-2 labeled match kinds (partial_relay,
// micro_detour): every lifecycle transition lands one append-only
// match_events row — the training data for a future ranking model. Nothing
// consumes these yet; keep writes best-effort and OFF the hot path
// (fire-and-forget from call sites via safeGo when latency matters).

// logMatchEventKnownKind records an event when the kind is already in hand
// (offer creation). No-ops for round-1 kinds — corridor volume would drown
// the signal we actually want to label.
func logMatchEventKnownKind(requestID, matchKind, event string, meta map[string]any) {
	if matchKind != "partial_relay" && matchKind != "micro_detour" {
		return
	}
	var metaJSON []byte
	if meta != nil {
		metaJSON, _ = json.Marshal(meta)
	}
	if _, err := db.DB.Exec(context.Background(), `
		INSERT INTO match_events (request_id, match_kind, event, meta)
		VALUES ($1, $2, $3, $4)
	`, requestID, matchKind, event, metaJSON); err != nil {
		log.Printf("match_events %s %s: %v", requestID, event, err)
	}
}

// logMatchEvent looks the kind up from the row, then records. The extra
// SELECT is the price of not threading match_kind through every status
// transition in the codebase; it only ever runs once per transition.
func logMatchEvent(requestID, event string, meta map[string]any) {
	var kind string
	if err := db.DB.QueryRow(context.Background(),
		`SELECT match_kind FROM ride_requests WHERE id = $1`, requestID,
	).Scan(&kind); err != nil {
		return
	}
	logMatchEventKnownKind(requestID, kind, event, meta)
}
