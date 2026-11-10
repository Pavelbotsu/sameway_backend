package models

import "time"

const (
	RoleDriver    = "driver"
	RolePassenger = "passenger"
)

// CurrentTermsVersion is the Terms of Service / Privacy Policy version users
// must have accepted. Bump it whenever either document changes materially:
// login/register then report needs_consent=true, the app re-prompts, and
// middleware.RequireConsent refuses ride/chat calls until the user accepts.
// The app carries the same number (lib/core/legal.dart kTermsVersion).
//
// Lives here rather than in handlers so middleware can use it without an
// import cycle.
//
// v2 (2026-11): full Terms/Privacy texts, date of birth required, driver
// declaration.
const CurrentTermsVersion = 2

const (
	StatusLooking    = "looking"
	StatusFull       = "full"
	StatusInProgress = "in_progress"
	StatusDone       = "done"
)

const (
	// RequestOffered is a driver-broadcast corridor offer awaiting the
	// passenger's accept. Passenger accept → pending (awaiting driver) →
	// accepted, so the driver always confirms who rides.
	RequestOffered    = "offered"
	RequestPending    = "pending"
	RequestAccepted   = "accepted"
	RequestInProgress = "in_progress"
	RequestDeclined   = "declined"
	RequestCancelled  = "cancelled"
	// RequestExpiredPassed is a terminal status set when the driver has
	// provably passed the pickup before the ride became active (re-validated
	// at accept and swept by the reaper). It sits OUTSIDE the active set in
	// the partial unique index, so it frees the passenger's match slot.
	RequestExpiredPassed = "expired_passed"
)

type User struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	Email           string     `json:"email"`
	Password        string     `json:"-"`
	Role            string     `json:"role"`
	FCMToken        string     `json:"fcm_token,omitempty"`
	PhotoURL        string     `json:"photo_url,omitempty"`
	Verified        bool       `json:"verified"`
	Bio             string     `json:"bio"`
	Phone           string     `json:"phone"`
	EmailVerifiedAt *time.Time `json:"email_verified_at,omitempty"`
	CarMake         string     `json:"car_make,omitempty"`
	CarModel        string     `json:"car_model,omitempty"`
	CarColor        string     `json:"car_color,omitempty"`
	CarPlate        string     `json:"car_plate,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
}

type ActiveDriver struct {
	ID         string    `json:"id"`
	DriverID   string    `json:"driver_id"`
	OriginLat  float64   `json:"origin_lat"`
	OriginLng  float64   `json:"origin_lng"`
	DestLat    float64   `json:"destination_lat"`
	DestLng    float64   `json:"destination_lng"`
	RouteWKT   string    `json:"route_wkt,omitempty"`
	CorridorKm float64   `json:"corridor_km"`
	Seats      int       `json:"seats"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
}

type ActivePassenger struct {
	ID          string    `json:"id"`
	PassengerID string    `json:"passenger_id"`
	Lat         float64   `json:"lat"`
	Lng         float64   `json:"lng"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type RideRequest struct {
	ID                   string    `json:"id"`
	DriverID             string    `json:"driver_id"`
	PassengerID          string    `json:"passenger_id"`
	PickupLat            float64   `json:"pickup_lat"`
	PickupLng            float64   `json:"pickup_lng"`
	Status               string    `json:"status"`
	CreatedAt            time.Time `json:"created_at"`
	PassengerName        string    `json:"passenger_name,omitempty"`
	PassengerPhotoURL    string    `json:"passenger_photo_url,omitempty"`
	PassengerAvgRating   float64   `json:"passenger_avg_rating,omitempty"`
	PassengerRatingCount int       `json:"passenger_rating_count,omitempty"`
	// Only set once the ride is accepted (see GetDriverRequests).
	PassengerAge *int       `json:"passenger_age,omitempty"`
	PickupCode   string     `json:"pickup_code,omitempty"`
	PickedUpAt   *time.Time `json:"picked_up_at,omitempty"`
	// Round 2 match labeling. Detour* are only set for micro_detour rows —
	// the driver's request tile shows "adds ~X m / Y s" from these before
	// they confirm (never-silent disclosure).
	MatchKind    string   `json:"match_kind,omitempty"`
	DetourAddedM *float64 `json:"detour_added_m,omitempty"`
	DetourAddedS *float64 `json:"detour_added_s,omitempty"`

	// Where this passenger LEAVES the car, and how far they ride — computed on
	// read (see GetDriverRequests), so the driver can judge a request before
	// accepting it instead of seeing only a name and a rating.
	//
	// This is the on-route drop-off, NOT the passenger's true final
	// destination: for a corridor match it is the projection of their
	// destination onto the driver's existing route, which is both what the
	// driver actually signs up for and the version that doesn't publish a
	// stranger's home address to anyone who receives a request.
	//
	// All three are nil when the passenger declared no destination
	// (match_kind = 'corridor_weak') — there is nothing to project.
	DropoffLat *float64 `json:"dropoff_lat,omitempty"`
	DropoffLng *float64 `json:"dropoff_lng,omitempty"`
	CarryM     *float64 `json:"carry_m,omitempty"`
}

type WSMessage struct {
	Type    string      `json:"type"`
	Payload interface{} `json:"payload"`
}

type Message struct {
	ID string `json:"id"`
	// ConversationID keys the persistent (pair) thread. RideRequestID is
	// retained for messages sent through the old ride-scoped endpoints and
	// for historical rows; empty on new conversation-first messages.
	ConversationID string     `json:"conversation_id,omitempty"`
	RideRequestID  string     `json:"ride_request_id,omitempty"`
	SenderID       string     `json:"sender_id"`
	Content        string     `json:"content"`
	CreatedAt      time.Time  `json:"created_at"`
	ReadAt         *time.Time `json:"read_at,omitempty"`
}

// Conversation is one persistent chat thread per unordered user pair,
// independent of any single ride_request (Round 2 §3). Peer* fields are
// denormalized for the recent-conversations list.
type Conversation struct {
	ID             string     `json:"id"`
	PeerID         string     `json:"peer_id"`
	PeerName       string     `json:"peer_name"`
	PeerPhotoURL   string     `json:"peer_photo_url,omitempty"`
	LastMessage    string     `json:"last_message,omitempty"`
	LastMessageAt  *time.Time `json:"last_message_at,omitempty"`
	UnreadCount    int        `json:"unread_count"`
	HasRideHistory bool       `json:"has_ride_history"`
	CreatedAt      time.Time  `json:"created_at"`
}

type PlannedTrip struct {
	ID              string    `json:"id"`
	DriverID        string    `json:"driver_id"`
	DriverName      string    `json:"driver_name"`
	CarSummary      string    `json:"car_summary"`
	OriginName      string    `json:"origin_name"`
	OriginLat       float64   `json:"origin_lat"`
	OriginLng       float64   `json:"origin_lng"`
	DestinationName string    `json:"destination_name"`
	DestLat         float64   `json:"dest_lat"`
	DestLng         float64   `json:"dest_lng"`
	Seats           int       `json:"seats"`
	CorridorKm      float64   `json:"corridor_km"`
	DistanceKm      float64   `json:"distance_km"`
	PlannedAt       time.Time `json:"planned_at"`
	Status          string    `json:"status"`
	CreatedAt       time.Time `json:"created_at"`
}
