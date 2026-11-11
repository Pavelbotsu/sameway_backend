package handlers

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/Pavelbotsu/sameway_backend/config"
	"github.com/Pavelbotsu/sameway_backend/db"
)

// Data retention (Privacy Policy, "How long we keep data"). Each rule is a
// single statement so a failing one doesn't block the others. Periods come
// from config (RETENTION_*), defaults:
//
//   - ride history (ride requests, their ratings, chat and match events;
//     completed-trip records): 3 years after the ride was created;
//   - chat: 12 months after a conversation's last message;
//   - user reports and safety flags: 2 years;
//   - bug reports: 1 year;
//   - last known location of drivers/passengers who went quiet: 24 hours;
//   - planned trips: 30 days after their planned time;
//   - reset / verification codes: once expired (reset rows are kept for the
//     24 h daily-cap window).
//
// Rides still in progress are never touched: only finished statuses are
// deleted, and only long after they ended.
type retentionRule struct {
	name string
	sql  string
	arg  func() any
}

var retentionRules = []retentionRule{
	{"ride_requests", `
		DELETE FROM ride_requests
		WHERE status IN ('done','cancelled','declined','expired')
		  AND created_at < NOW() - make_interval(days => $1)`,
		func() any { return config.C.RetentionRideDays }},
	{"completed_trips", `
		DELETE FROM completed_trips
		WHERE completed_at < NOW() - make_interval(days => $1)`,
		func() any { return config.C.RetentionRideDays }},
	{"conversations", `
		DELETE FROM conversations
		WHERE COALESCE(last_message_at, created_at) < NOW() - make_interval(days => $1)`,
		func() any { return config.C.RetentionChatDays }},
	// Ride-scoped messages from before conversations existed.
	{"legacy_messages", `
		DELETE FROM messages
		WHERE conversation_id IS NULL
		  AND created_at < NOW() - make_interval(days => $1)`,
		func() any { return config.C.RetentionChatDays }},
	{"user_reports", `
		DELETE FROM user_reports
		WHERE created_at < NOW() - make_interval(days => $1)`,
		func() any { return config.C.RetentionReportDays }},
	{"safety_flags", `
		UPDATE ride_requests
		SET safety_flagged_at = NULL, safety_flagged_by = NULL, safety_flag_reason = NULL
		WHERE safety_flagged_at < NOW() - make_interval(days => $1)`,
		func() any { return config.C.RetentionReportDays }},
	{"bug_reports", `
		DELETE FROM bug_reports
		WHERE created_at < NOW() - make_interval(days => $1)`,
		func() any { return config.C.RetentionBugReportDays }},
	{"stale_passengers", `
		DELETE FROM active_passengers ap
		WHERE ap.updated_at < NOW() - make_interval(hours => $1)
		  AND NOT EXISTS (SELECT 1 FROM ride_requests rr
		                  WHERE rr.passenger_id = ap.passenger_id
		                    AND rr.status IN ('offered','pending','accepted','in_progress'))`,
		func() any { return config.C.RetentionLiveLocationHours }},
	{"stale_drivers", `
		DELETE FROM active_drivers ad
		WHERE COALESCE(ad.current_location_at, ad.created_at) < NOW() - make_interval(hours => $1)
		  AND NOT EXISTS (SELECT 1 FROM ride_requests rr
		                  WHERE rr.driver_id = ad.driver_id
		                    AND rr.status IN ('offered','pending','accepted','in_progress'))`,
		func() any { return config.C.RetentionLiveLocationHours }},
	{"planned_trips", `
		DELETE FROM planned_trips
		WHERE planned_at < NOW() - make_interval(days => $1)`,
		func() any { return config.C.RetentionPlannedTripDays }},
	{"password_resets", `
		DELETE FROM password_resets
		WHERE expires_at < NOW() AND window_started_at < NOW() - INTERVAL '24 hours'`,
		nil},
	{"email_verifications", `
		DELETE FROM email_verifications WHERE expires_at < NOW()`,
		nil},
	{"ride_shares", `
		DELETE FROM ride_shares WHERE expires_at < NOW()`,
		nil},
}

// RunRetentionOnce applies every rule once and returns rows affected per
// rule. Exported for tests and the admin tool.
func RunRetentionOnce(ctx context.Context) map[string]int64 {
	out := make(map[string]int64, len(retentionRules))
	for _, r := range retentionRules {
		var args []any
		if r.arg != nil {
			args = append(args, r.arg())
		}
		tag, err := db.DB.Exec(ctx, r.sql, args...)
		if err != nil {
			log.Printf("retention %s: %v", r.name, err)
			continue
		}
		out[r.name] = tag.RowsAffected()
	}
	return out
}

var (
	retentionMu      sync.Mutex
	retentionLastRun time.Time
)

// runRetentionDaily is called by the reaper every minute and runs the rules
// at most once a day (first run shortly after start-up).
func runRetentionDaily() {
	retentionMu.Lock()
	due := time.Since(retentionLastRun) >= 24*time.Hour
	if due {
		retentionLastRun = time.Now()
	}
	retentionMu.Unlock()
	if !due {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	counts := RunRetentionOnce(ctx)
	var total int64
	for _, n := range counts {
		total += n
	}
	if total > 0 {
		log.Printf("retention: removed %v", counts)
	}
}
