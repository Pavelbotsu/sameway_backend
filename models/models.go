package models

import "time"

const (
	RoleDriver    = "driver"
	RolePassenger = "passenger"
)

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
	ID                   string     `json:"id"`
	DriverID             string     `json:"driver_id"`
	PassengerID          string     `json:"passenger_id"`
	PickupLat            float64    `json:"pickup_lat"`
	PickupLng            float64    `json:"pickup_lng"`
	Status               string     `json:"status"`
	CreatedAt            time.Time  `json:"created_at"`
	PassengerName        string     `json:"passenger_name,omitempty"`
	PassengerPhotoURL    string     `json:"passenger_photo_url,omitempty"`
	PassengerAvgRating   float64    `json:"passenger_avg_rating,omitempty"`
	PassengerRatingCount int        `json:"passenger_rating_count,omitempty"`
	PickupCode           string     `json:"pickup_code,omitempty"`
	PickedUpAt           *time.Time `json:"picked_up_at,omitempty"`
}

type WSMessage struct {
	Type    string      `json:"type"`
	Payload interface{} `json:"payload"`
}

type Message struct {
	ID            string     `json:"id"`
	RideRequestID string     `json:"ride_request_id"`
	SenderID      string     `json:"sender_id"`
	Content       string     `json:"content"`
	CreatedAt     time.Time  `json:"created_at"`
	ReadAt        *time.Time `json:"read_at,omitempty"`
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
