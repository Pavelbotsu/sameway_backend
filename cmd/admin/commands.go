package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Each command writes plain text to w so it works over `docker compose exec`
// and in tests.

func listReports(ctx context.Context, db *pgxpool.Pool, w io.Writer, limit int) error {
	rows, err := db.Query(ctx, `
		SELECT r.created_at, rep.email, tgt.email, tgt.suspended_at IS NOT NULL, r.reason
		FROM user_reports r
		JOIN users rep ON rep.id = r.reporter_id
		JOIN users tgt ON tgt.id = r.reported_id
		ORDER BY r.created_at DESC LIMIT $1`, limit)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "WHEN\tREPORTER\tREPORTED\tSUSPENDED\tREASON")
	for rows.Next() {
		var at time.Time
		var reporter, reported, reason string
		var suspended bool
		if err := rows.Scan(&at, &reporter, &reported, &suspended, &reason); err != nil {
			return err
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%v\t%s\n", at.Format(time.DateTime), reporter, reported, suspended, oneLine(reason))
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return tw.Flush()
}

func listFlags(ctx context.Context, db *pgxpool.Pool, w io.Writer, limit int) error {
	rows, err := db.Query(ctx, `
		SELECT rr.safety_flagged_at, rr.id::text, flagger.email, d.email, p.email, rr.status,
		       COALESCE(rr.safety_flag_reason, '')
		FROM ride_requests rr
		JOIN users flagger ON flagger.id = rr.safety_flagged_by
		JOIN users d ON d.id = rr.driver_id
		JOIN users p ON p.id = rr.passenger_id
		WHERE rr.safety_flagged_at IS NOT NULL
		ORDER BY rr.safety_flagged_at DESC LIMIT $1`, limit)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "WHEN\tRIDE\tFLAGGED BY\tDRIVER\tPASSENGER\tSTATUS\tREASON")
	for rows.Next() {
		var at time.Time
		var id, by, driver, pax, status, reason string
		if err := rows.Scan(&at, &id, &by, &driver, &pax, &status, &reason); err != nil {
			return err
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", at.Format(time.DateTime), id, by, driver, pax, status, oneLine(reason))
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return tw.Flush()
}

func listBugs(ctx context.Context, db *pgxpool.Pool, w io.Writer, limit int) error {
	rows, err := db.Query(ctx, `
		SELECT b.created_at, u.email, b.title, COALESCE(b.app_version, ''), b.body
		FROM bug_reports b JOIN users u ON u.id = b.user_id
		ORDER BY b.created_at DESC LIMIT $1`, limit)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "WHEN\tUSER\tAPP\tTITLE\tBODY")
	for rows.Next() {
		var at time.Time
		var email, title, version, body string
		if err := rows.Scan(&at, &email, &title, &version, &body); err != nil {
			return err
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", at.Format(time.DateTime), email, version, oneLine(title), oneLine(body))
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return tw.Flush()
}

// showUser prints one account: identity, status and activity counts.
func showUser(ctx context.Context, db *pgxpool.Pool, w io.Writer, who string) error {
	var (
		id, name, email, role string
		created               time.Time
		suspendedAt           *time.Time
		suspendedReason       *string
		deletedAt             *time.Time
		declared              *time.Time
		rides, reportsAgainst int
		reportsBy, flags      int
		avg                   float64
		ratings               int
	)
	err := db.QueryRow(ctx, `
		SELECT u.id::text, u.name, u.email, u.role, u.created_at, u.suspended_at, u.suspended_reason,
		       u.deleted_at, u.driver_declared_at,
		       (SELECT COUNT(*) FROM ride_requests WHERE (driver_id = u.id OR passenger_id = u.id) AND status = 'done'),
		       (SELECT COUNT(*) FROM user_reports WHERE reported_id = u.id),
		       (SELECT COUNT(*) FROM user_reports WHERE reporter_id = u.id),
		       (SELECT COUNT(*) FROM ride_requests WHERE (driver_id = u.id OR passenger_id = u.id) AND safety_flagged_at IS NOT NULL),
		       (SELECT COALESCE(AVG(stars), 0)::float8 FROM ratings WHERE rated_id = u.id),
		       (SELECT COUNT(*) FROM ratings WHERE rated_id = u.id)
		FROM users u
		WHERE u.email = lower($1) OR u.id::text = $1`, who,
	).Scan(&id, &name, &email, &role, &created, &suspendedAt, &suspendedReason, &deletedAt, &declared,
		&rides, &reportsAgainst, &reportsBy, &flags, &avg, &ratings)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("no user %q", who)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "id:           %s\nname:         %s\nemail:        %s\nrole:         %s\ncreated:      %s\n",
		id, name, email, role, created.Format(time.DateTime))
	switch {
	case deletedAt != nil:
		fmt.Fprintf(w, "status:       DELETED %s\n", deletedAt.Format(time.DateTime))
	case suspendedAt != nil:
		fmt.Fprintf(w, "status:       SUSPENDED %s (%s)\n", suspendedAt.Format(time.DateTime), deref(suspendedReason))
	default:
		fmt.Fprintln(w, "status:       active")
	}
	if declared != nil {
		fmt.Fprintf(w, "driver decl:  %s\n", declared.Format(time.DateTime))
	}
	fmt.Fprintf(w, "rides done:   %d\nrating:       %.1f (%d)\nreported:     %d times\nreports by:   %d\nsafety flags: %d\n",
		rides, avg, ratings, reportsAgainst, reportsBy, flags)
	return nil
}

// setSuspended suspends (reason required) or unsuspends an account. The API
// refuses it at once and open sockets close within a minute
// (middleware.Auth, handlers.WSAccountRecheck).
func setSuspended(ctx context.Context, db *pgxpool.Pool, w io.Writer, who string, suspend bool, reason string) error {
	var n int64
	if suspend {
		if strings.TrimSpace(reason) == "" {
			return errors.New("a --reason is required to suspend")
		}
		tag, err := db.Exec(ctx, `
			UPDATE users SET suspended_at = NOW(), suspended_reason = $2
			WHERE (email = lower($1) OR id::text = $1) AND deleted_at IS NULL`, who, reason)
		if err != nil {
			return err
		}
		n = tag.RowsAffected()
	} else {
		tag, err := db.Exec(ctx, `
			UPDATE users SET suspended_at = NULL, suspended_reason = NULL
			WHERE (email = lower($1) OR id::text = $1) AND deleted_at IS NULL`, who)
		if err != nil {
			return err
		}
		n = tag.RowsAffected()
	}
	if n == 0 {
		return fmt.Errorf("no active user %q", who)
	}
	if suspend {
		fmt.Fprintf(w, "suspended %s: %s\n", who, reason)
	} else {
		fmt.Fprintf(w, "unsuspended %s\n", who)
	}
	return nil
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 80 {
		return string(r[:77]) + "..."
	}
	return s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
