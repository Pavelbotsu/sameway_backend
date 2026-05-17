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
	RequestPending   = "pending"
	RequestAccepted  = "accepted"
	RequestDeclined  = "declined"
	RequestCancelled = "cancelled"
)

type User struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Email     string    `json:"email"`
	Password  string    `json:"-"`
	Role      string    `json:"role"`
	FCMToken  string    `json:"fcm_token,omitempty"`
	CreatedAt time.Time `json:"created_at"`
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
	ID          string    `json:"id"`
	DriverID    string    `json:"driver_id"`
	PassengerID string    `json:"passenger_id"`
	PickupLat   float64   `json:"pickup_lat"`
	PickupLng   float64   `json:"pickup_lng"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
}

type WSMessage struct {
	Type    string      `json:"type"`
	Payload interface{} `json:"payload"`
}
