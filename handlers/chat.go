package handlers

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Pavelbotsu/sameway_backend/config"
	"github.com/Pavelbotsu/sameway_backend/db"
	"github.com/Pavelbotsu/sameway_backend/models"
	"github.com/Pavelbotsu/sameway_backend/services"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

// Chat is keyed to persistent conversations — one per unordered user pair,
// canonicalized as (user_a < user_b) via LEAST/GREATEST in EVERY write and
// lookup (Round 2 §3). History survives across rides; the old per-ride
// endpoints remain as a one-release compat shim that resolves a
// ride_request_id to the pair's conversation.

// ── Pair helpers ─────────────────────────────────────────────────────────────

// getOrCreateConversation returns the pair's conversation id, minting the row
// if absent. The no-op DO UPDATE makes RETURNING yield the id on conflict too.
func getOrCreateConversation(ctx context.Context, userX, userY string) (string, error) {
	var id string
	err := db.DB.QueryRow(ctx, `
		INSERT INTO conversations (user_a, user_b)
		VALUES (LEAST($1::uuid, $2::uuid), GREATEST($1::uuid, $2::uuid))
		ON CONFLICT (user_a, user_b) DO UPDATE SET user_a = conversations.user_a
		RETURNING id
	`, userX, userY).Scan(&id)
	return id, err
}

// conversationPeer loads the conversation and returns the OTHER participant.
// ok=false when the row is missing or userID isn't a participant.
func conversationPeer(ctx context.Context, conversationID, userID string) (peerID string, ok bool) {
	var a, b string
	if err := db.DB.QueryRow(ctx,
		`SELECT user_a, user_b FROM conversations WHERE id = $1`, conversationID,
	).Scan(&a, &b); err != nil {
		return "", false
	}
	switch userID {
	case a:
		return b, true
	case b:
		return a, true
	}
	return "", false
}

// isBlockedPair reports whether either side has blocked the other.
func isBlockedPair(ctx context.Context, x, y string) bool {
	var blocked bool
	_ = db.DB.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM user_blocks
			WHERE (blocker_id = $1 AND blocked_id = $2)
			   OR (blocker_id = $2 AND blocked_id = $1)
		)
	`, x, y).Scan(&blocked)
	return blocked
}

// pairHasRideRequest reports whether ANY ride_request (any status, either
// role orientation) ever existed between the two users. This is the "we've
// already been matched at least once" conversation gate.
func pairHasRideRequest(ctx context.Context, x, y string) bool {
	var exists bool
	_ = db.DB.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM ride_requests
			WHERE (driver_id = $1 AND passenger_id = $2)
			   OR (driver_id = $2 AND passenger_id = $1)
		)
	`, x, y).Scan(&exists)
	return exists
}

// pairHasCompletedRide reports whether the two users ever finished a ride
// together. Pre-ride pairs get the first-message rate limit; post-ride pairs
// chat freely.
func pairHasCompletedRide(ctx context.Context, x, y string) bool {
	var exists bool
	_ = db.DB.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM ride_requests
			WHERE status = 'done'
			  AND ((driver_id = $1 AND passenger_id = $2)
			    OR (driver_id = $2 AND passenger_id = $1))
		)
	`, x, y).Scan(&exists)
	return exists
}

// ── Pre-match candidate gate ─────────────────────────────────────────────────

// canOpenConversation enforces the pre-match rule: you may only start a
// conversation with someone who currently appears in YOUR algorithmic
// candidate list (Round 2 §3) — no arbitrary/broadcast messaging.
//   - Any historical ride_request between the pair → allowed.
//   - Passenger caller → the driver must be visible in their nearby-drivers
//     view (looking, fresh route-set, within ChatCandidateRadiusKm of the
//     caller's active location).
//   - Driver caller → the passenger must currently match the caller's own
//     corridor (same gates as a corridor offer).
func canOpenConversation(ctx context.Context, callerID, callerRole, peerID string) bool {
	if pairHasRideRequest(ctx, callerID, peerID) {
		return true
	}
	switch callerRole {
	case models.RolePassenger:
		var visible bool
		_ = db.DB.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1
				FROM active_drivers ad
				JOIN active_passengers ap ON ap.passenger_id = $1
				WHERE ad.driver_id = $2
				  AND ad.status = 'looking'
				  AND ad.created_at > NOW() - INTERVAL '30 minutes'
				  AND ST_DWithin(ad.origin::geography, ap.location, $3::float8 * 1000)
			)
		`, callerID, peerID, config.C.ChatCandidateRadiusKm).Scan(&visible)
		return visible
	case models.RoleDriver:
		var routeWKT string
		var destLat, destLng, corridorKm float64
		err := db.DB.QueryRow(ctx, `
			SELECT COALESCE(ST_AsText(route), ''),
			       ST_Y(destination::geometry), ST_X(destination::geometry),
			       corridor_km
			FROM active_drivers
			WHERE driver_id = $1 AND status = 'looking'
		`, callerID).Scan(&routeWKT, &destLat, &destLng, &corridorKm)
		if err != nil || routeWKT == "" {
			return false
		}
		matches, err := services.FindCorridorMatchForPassenger(
			callerID, routeWKT, corridorKm, destLat, destLng, peerID)
		return err == nil && len(matches) > 0
	}
	return false
}

// ── Pre-ride rate limit (in-process, single instance — see README) ──────────

var (
	chatRateMu      sync.Mutex
	chatRateWindows = map[string]*chatRateWindow{}
)

type chatRateWindow struct {
	start time.Time
	count int
}

// allowPreRideMessage is a fixed one-minute window per (sender, conversation).
func allowPreRideMessage(senderID, conversationID string) bool {
	key := senderID + ":" + conversationID
	now := time.Now()
	chatRateMu.Lock()
	defer chatRateMu.Unlock()
	w, ok := chatRateWindows[key]
	if !ok || now.Sub(w.start) >= time.Minute {
		chatRateWindows[key] = &chatRateWindow{start: now, count: 1}
		return true
	}
	if w.count >= config.C.ChatPreRideMaxPerMin {
		return false
	}
	w.count++
	return true
}

// sweepChatRateLimit drops stale windows; called from the reaper tick.
func sweepChatRateLimit() {
	cutoff := time.Now().Add(-time.Hour)
	chatRateMu.Lock()
	for k, w := range chatRateWindows {
		if w.start.Before(cutoff) {
			delete(chatRateWindows, k)
		}
	}
	chatRateMu.Unlock()
}

// ── Conversation endpoints ───────────────────────────────────────────────────

// OpenConversation gets-or-creates the conversation with peer_id, gated by
// the candidate rule above. POST /chat/conversations {peer_id}.
func OpenConversation(c *gin.Context) {
	var req struct {
		PeerID string `json:"peer_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	callerID := c.GetString("user_id")
	callerRole := c.GetString("role")
	ctx := c.Request.Context()

	if req.PeerID == callerID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot message yourself"})
		return
	}
	if isBlockedPair(ctx, callerID, req.PeerID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "messaging unavailable for this user"})
		return
	}
	if !canOpenConversation(ctx, callerID, callerRole, req.PeerID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "you can only message your current match candidates"})
		return
	}

	convID, err := getOrCreateConversation(ctx, callerID, req.PeerID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	var peerName, peerPhoto string
	_ = db.DB.QueryRow(ctx,
		`SELECT name, COALESCE(photo_url,'') FROM users WHERE id = $1`, req.PeerID,
	).Scan(&peerName, &peerPhoto)

	c.JSON(http.StatusOK, gin.H{
		"conversation_id": convID,
		"peer_id":         req.PeerID,
		"peer_name":       peerName,
		"peer_photo_url":  peerPhoto,
	})
}

// ListConversations returns the caller's recent conversations, newest
// activity first. GET /chat/conversations.
func ListConversations(c *gin.Context) {
	userID := c.GetString("user_id")
	rows, err := db.DB.Query(c.Request.Context(), `
		SELECT c.id,
		       CASE WHEN c.user_a = $1 THEN c.user_b ELSE c.user_a END,
		       u.name, COALESCE(u.photo_url, ''),
		       COALESCE(lm.content, ''), c.last_message_at, c.created_at,
		       COALESCE(unread.cnt, 0),
		       EXISTS (
		           SELECT 1 FROM ride_requests rr
		           WHERE (rr.driver_id = c.user_a AND rr.passenger_id = c.user_b)
		              OR (rr.driver_id = c.user_b AND rr.passenger_id = c.user_a)
		       )
		FROM conversations c
		JOIN users u ON u.id = CASE WHEN c.user_a = $1 THEN c.user_b ELSE c.user_a END
		LEFT JOIN LATERAL (
			SELECT content FROM messages m
			WHERE m.conversation_id = c.id
			ORDER BY m.created_at DESC LIMIT 1
		) lm ON TRUE
		LEFT JOIN LATERAL (
			SELECT COUNT(*) AS cnt FROM messages m
			WHERE m.conversation_id = c.id AND m.sender_id <> $1 AND m.read_at IS NULL
		) unread ON TRUE
		WHERE $1 IN (c.user_a, c.user_b)
		ORDER BY c.last_message_at DESC NULLS LAST, c.created_at DESC
		LIMIT 100
	`, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	defer rows.Close()

	out := make([]models.Conversation, 0)
	for rows.Next() {
		var conv models.Conversation
		if err := rows.Scan(&conv.ID, &conv.PeerID, &conv.PeerName, &conv.PeerPhotoURL,
			&conv.LastMessage, &conv.LastMessageAt, &conv.CreatedAt,
			&conv.UnreadCount, &conv.HasRideHistory); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "scan error"})
			return
		}
		out = append(out, conv)
	}
	c.JSON(http.StatusOK, out)
}

// GetConversationMessages returns full history for one conversation.
// GET /chat/conversations/:id/messages.
func GetConversationMessages(c *gin.Context) {
	conversationID := c.Param("id")
	userID := c.GetString("user_id")

	if _, ok := conversationPeer(c.Request.Context(), conversationID, userID); !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "conversation not found"})
		return
	}
	writeConversationMessages(c, conversationID)
}

// messagePageSize bounds one page of chat history.
const messagePageSize = 50

func writeConversationMessages(c *gin.Context, conversationID string) {
	// Paginated newest-first, then reversed for the client.
	//
	// This was previously unbounded (`ORDER BY created_at ASC`, no LIMIT).
	// Conversations between pairs with ride history are kept indefinitely by
	// the retention policy and messages can be 2000 characters each, so an
	// established pair eventually made every chat-open download the entire
	// thread.
	//
	// `before` is an optional RFC3339 cursor: pass the created_at of the oldest
	// message you hold to fetch the page before it.
	before := c.Query("before")
	rows, err := db.DB.Query(c.Request.Context(), `
		SELECT id, conversation_id, COALESCE(ride_request_id::text, ''),
		       sender_id, content, created_at, read_at
		FROM messages
		WHERE conversation_id = $1
		  AND ($2::timestamptz IS NULL OR created_at < $2::timestamptz)
		ORDER BY created_at DESC
		LIMIT $3
	`, conversationID, nullIfEmpty(before), messagePageSize)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	defer rows.Close()

	msgs := make([]models.Message, 0, messagePageSize)
	for rows.Next() {
		var m models.Message
		if err := rows.Scan(&m.ID, &m.ConversationID, &m.RideRequestID,
			&m.SenderID, &m.Content, &m.CreatedAt, &m.ReadAt); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "scan error"})
			return
		}
		msgs = append(msgs, m)
	}
	if err := rows.Err(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	// The query runs newest-first (so LIMIT takes the most recent page); the
	// client renders oldest-first, and every existing caller assumes ascending
	// order, so flip before returning.
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	c.JSON(http.StatusOK, msgs)
}

// nullIfEmpty maps "" to a nil interface so a $n::timestamptz parameter binds
// as SQL NULL (the "no cursor" case) rather than failing to parse "".
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// resolveConversation turns the request's conversation_id OR ride_request_id
// (compat shim) into (conversationID, peerID). Emits the HTTP error itself
// and returns ok=false on failure.
func resolveConversation(c *gin.Context, conversationID, rideRequestID, userID string) (convID, peerID, resolvedRideID string, ok bool) {
	ctx := c.Request.Context()
	if conversationID != "" {
		peer, isMember := conversationPeer(ctx, conversationID, userID)
		if !isMember {
			c.JSON(http.StatusNotFound, gin.H{"error": "conversation not found"})
			return "", "", "", false
		}
		return conversationID, peer, "", true
	}
	if rideRequestID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "conversation_id or ride_request_id required"})
		return "", "", "", false
	}
	// Old clients: ride → pair → conversation. No liveness gate anymore —
	// the conversation persists past the ride.
	var driverID, passengerID string
	err := db.DB.QueryRow(ctx,
		`SELECT driver_id, passenger_id FROM ride_requests WHERE id = $1`, rideRequestID,
	).Scan(&driverID, &passengerID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "ride request not found"})
		return "", "", "", false
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return "", "", "", false
	}
	if userID != driverID && userID != passengerID {
		c.JSON(http.StatusForbidden, gin.H{"error": "not part of this ride"})
		return "", "", "", false
	}
	id, err := getOrCreateConversation(ctx, driverID, passengerID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return "", "", "", false
	}
	peer := passengerID
	if userID == passengerID {
		peer = driverID
	}
	return id, peer, rideRequestID, true
}

// ── Messaging ────────────────────────────────────────────────────────────────

// SendMessage posts into a conversation. Accepts conversation_id (new
// clients) or ride_request_id (compat shim). POST /chat/send.
func SendMessage(c *gin.Context) {
	var req struct {
		ConversationID string `json:"conversation_id"`
		RideRequestID  string `json:"ride_request_id"`
		Content        string `json:"content" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	senderID := c.GetString("user_id")
	req.Content = strings.TrimSpace(req.Content)
	if req.Content == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "empty message"})
		return
	}
	if len(req.Content) > 2000 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "message too long (max 2000 characters)"})
		return
	}

	convID, recipientID, rideID, ok := resolveConversation(c, req.ConversationID, req.RideRequestID, senderID)
	if !ok {
		return
	}
	ctx := c.Request.Context()

	if isBlockedPair(ctx, senderID, recipientID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "messaging unavailable for this user"})
		return
	}
	// Pre-ride pairs are rate-limited on messages (anti-spam for the
	// pre-match "let's confirm before requesting" flow).
	if !pairHasCompletedRide(ctx, senderID, recipientID) && !allowPreRideMessage(senderID, convID) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "sending too fast — wait a moment"})
		return
	}

	var msgID string
	var createdAt time.Time
	err := db.DB.QueryRow(ctx, `
		INSERT INTO messages (conversation_id, ride_request_id, sender_id, content)
		VALUES ($1, NULLIF($2, '')::uuid, $3, $4)
		RETURNING id, created_at
	`, convID, rideID, senderID, req.Content).Scan(&msgID, &createdAt)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	if _, err := db.DB.Exec(ctx,
		`UPDATE conversations SET last_message_at = $1 WHERE id = $2`, createdAt, convID,
	); err != nil {
		// Non-fatal: the list just sorts this thread slightly stale.
	}

	msg := models.Message{
		ID:             msgID,
		ConversationID: convID,
		RideRequestID:  rideID,
		SenderID:       senderID,
		Content:        req.Content,
		CreatedAt:      createdAt,
	}

	Hub.SendToUser(recipientID, "chat_message", msg)

	// Mirror the WS push via FCM so a backgrounded recipient still gets the
	// notification. Best-effort: if the recipient has no token or FCM isn't
	// configured, NotifyChatMessage is a no-op.
	var recipientToken, senderName string
	_ = db.DB.QueryRow(context.Background(), `
		SELECT (SELECT COALESCE(fcm_token, '') FROM users WHERE id = $1),
		       (SELECT COALESCE(name, '')      FROM users WHERE id = $2)
	`, recipientID, senderID).Scan(&recipientToken, &senderName)
	go safeGo("fcm-chat-notify", func() {
		services.NotifyChatMessage(recipientToken, senderID, senderName, convID, rideID, req.Content)
	})()

	c.JSON(http.StatusOK, msg)
}

// GetMessages is the compat shim for old clients: resolves the ride to the
// pair's conversation and returns the FULL pair history (a superset of the
// old per-ride thread). GET /chat/:request_id.
func GetMessages(c *gin.Context) {
	requestID := c.Param("request_id")
	userID := c.GetString("user_id")

	convID, _, _, ok := resolveConversation(c, "", requestID, userID)
	if !ok {
		return
	}
	writeConversationMessages(c, convID)
}

// TypingStatus broadcasts a transient "X is typing" hint to the other party
// over WebSocket. No persistence — the client debounces at the keystroke layer
// and stops sending after ~3 s of inactivity. The recipient's UI shows the
// pill for a fixed window after each "typing:true" event.
func TypingStatus(c *gin.Context) {
	var req struct {
		ConversationID string `json:"conversation_id"`
		RideRequestID  string `json:"ride_request_id"`
		Typing         bool   `json:"typing"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	senderID := c.GetString("user_id")
	convID, recipientID, rideID, ok := resolveConversation(c, req.ConversationID, req.RideRequestID, senderID)
	if !ok {
		return
	}

	Hub.SendToUser(recipientID, "chat_typing", gin.H{
		"conversation_id": convID,
		"ride_request_id": rideID,
		"user_id":         senderID,
		"typing":          req.Typing,
	})
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// MarkRead stamps every message in the conversation that was sent by the
// OTHER party as read (now()) and notifies the sender so their ✓ → ✓✓ flips.
// Idempotent; re-calling on already-read messages is a no-op.
func MarkRead(c *gin.Context) {
	var req struct {
		ConversationID string `json:"conversation_id"`
		RideRequestID  string `json:"ride_request_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	readerID := c.GetString("user_id")
	convID, senderID, rideID, ok := resolveConversation(c, req.ConversationID, req.RideRequestID, readerID)
	if !ok {
		return
	}

	now := time.Now()
	rows, err := db.DB.Query(c.Request.Context(), `
		UPDATE messages
		SET read_at = $1
		WHERE conversation_id = $2
		  AND sender_id != $3
		  AND read_at IS NULL
		RETURNING id
	`, now, convID, readerID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		c.JSON(http.StatusOK, gin.H{"updated": 0})
		return
	}

	Hub.SendToUser(senderID, "message_read", gin.H{
		"conversation_id": convID,
		"ride_request_id": rideID,
		"message_ids":     ids,
		"read_at":         now,
	})
	c.JSON(http.StatusOK, gin.H{"updated": len(ids)})
}

// ── Abuse controls ───────────────────────────────────────────────────────────

// BlockUser stops messaging in both directions. POST /chat/block {user_id}.
func BlockUser(c *gin.Context) {
	var req struct {
		UserID string `json:"user_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	blockerID := c.GetString("user_id")
	if req.UserID == blockerID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot block yourself"})
		return
	}
	if _, err := db.DB.Exec(c.Request.Context(), `
		INSERT INTO user_blocks (blocker_id, blocked_id)
		VALUES ($1, $2) ON CONFLICT DO NOTHING
	`, blockerID, req.UserID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// UnblockUser lifts the caller's own block. DELETE /chat/block/:user_id.
func UnblockUser(c *gin.Context) {
	blockerID := c.GetString("user_id")
	if _, err := db.DB.Exec(c.Request.Context(),
		`DELETE FROM user_blocks WHERE blocker_id = $1 AND blocked_id = $2`,
		blockerID, c.Param("user_id"),
	); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ReportUser files an abuse report (audit trail for ops; no automated
// action). POST /chat/report {user_id, conversation_id?, reason}.
func ReportUser(c *gin.Context) {
	var req struct {
		UserID         string `json:"user_id" binding:"required"`
		ConversationID string `json:"conversation_id"`
		Reason         string `json:"reason"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	reporterID := c.GetString("user_id")
	if len(req.Reason) > 1000 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "reason too long (max 1000 characters)"})
		return
	}
	if _, err := db.DB.Exec(c.Request.Context(), `
		INSERT INTO user_reports (reporter_id, reported_id, conversation_id, reason)
		VALUES ($1, $2, NULLIF($3, '')::uuid, $4)
	`, reporterID, req.UserID, req.ConversationID, strings.TrimSpace(req.Reason)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ── Retention ────────────────────────────────────────────────────────────────

// ReapIdleConversationsOnce enforces the retention policy (Round 2 §3):
// conversations between pairs that NEVER completed a ride are deleted —
// messages cascade — after ChatRetentionNoRideDays of inactivity (last
// message, or creation when empty). Pairs with a completed ride keep their
// history indefinitely. Exported so tests invoke it synchronously.
func ReapIdleConversationsOnce() {
	_, err := db.DB.Exec(context.Background(), `
		DELETE FROM conversations c
		WHERE COALESCE(c.last_message_at, c.created_at)
		      < NOW() - make_interval(days => $1)
		  AND NOT EXISTS (
			SELECT 1 FROM ride_requests rr
			WHERE rr.status = 'done'
			  AND ((rr.driver_id = c.user_a AND rr.passenger_id = c.user_b)
			    OR (rr.driver_id = c.user_b AND rr.passenger_id = c.user_a))
		  )
	`, config.C.ChatRetentionNoRideDays)
	if err != nil {
		log.Printf("conversation retention sweep: %v", err)
	}
}
