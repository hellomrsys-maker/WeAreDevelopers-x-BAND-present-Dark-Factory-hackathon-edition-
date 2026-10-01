package contracts

import "time"

// User represents an authenticated account
type User struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	Password    string `json:"password,omitempty"` // Hashed, omitted in API responses
	DisplayName string `json:"display_name"`
}

// OpeningHour defines operating hours for a weekday
type OpeningHour struct {
	Weekday string `json:"weekday"` // mon, tue, wed, thu, fri, sat, sun
	Opens   string `json:"opens"`   // HH:MM 24-hour
	Closes  string `json:"closes"`  // HH:MM 24-hour
}

// Table represents a dining table in a restaurant
type Table struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Capacity int    `json:"capacity"`
}

// Restaurant defines the properties and policies of a venue
type Restaurant struct {
	ID                         string        `json:"id"`
	Name                       string        `json:"name"`
	Timezone                   string        `json:"timezone"`
	SlotMinutes                int           `json:"slot_minutes"`
	ReservationDurationMinutes int           `json:"reservation_duration_minutes"`
	CancellationCutoffMinutes  int           `json:"cancellation_cutoff_minutes"`
	OpeningHours               []OpeningHour `json:"opening_hours"`
	Tables                     []Table       `json:"tables"`
}

// Reservation represents a booked or cancelled table assignment
type Reservation struct {
	ID            string    `json:"reservation_id"`
	Reference     string    `json:"reference"`
	RestaurantID  string    `json:"restaurant_id"`
	TableID       string    `json:"table_id"`
	UserID        string    `json:"user_id,omitempty"`
	PartySize     int       `json:"party_size"`
	Status        string    `json:"status"` // confirmed or cancelled
	StartsAtLocal string    `json:"starts_at_local"`
	StartsAtUTC   int64     `json:"starts_at_utc,omitempty"`
	EndsAtUTC     int64     `json:"ends_at_utc,omitempty"`
	CreatedAtUTC  int64     `json:"created_at_utc,omitempty"`
	StartsAt      time.Time `json:"-"`
	EndsAt        time.Time `json:"-"`
	CreatedAt     time.Time `json:"-"`
}

// AvailabilitySlot represents an available interval on the booking grid
type AvailabilitySlot struct {
	StartsAtLocal     string   `json:"starts_at_local"`
	StartsAt          string   `json:"starts_at"`
	AvailableTableIDs []string `json:"available_table_ids"`
}

// ReservationMoveRequest defines a target change for an existing reservation
type ReservationMoveRequest struct {
	Reference     string  `json:"reference"`
	TableID       *string `json:"table_id,omitempty"`
	StartsAtLocal *string `json:"starts_at_local,omitempty"`
	PartySize     *int    `json:"party_size,omitempty"`
}

// FixtureData is used by POST /_test/reset
type FixtureData struct {
	Users        []User        `json:"users"`
	Restaurants  []Restaurant  `json:"restaurants"`
	Reservations []Reservation `json:"reservations"`
}

// ExportState represents the opaque serializable snapshot for /_test/export and /_test/import
type ExportPayload struct {
	Track         string      `json:"track"`
	FormatVersion int         `json:"format_version"`
	State         ExportState `json:"state"`
}

type ExportState struct {
	Users        []User            `json:"users"`
	Tokens       map[string]string `json:"tokens"` // token -> user_id
	Restaurants  []Restaurant      `json:"restaurants"`
	Reservations []Reservation     `json:"reservations"`
	Idempotency  []IdempotencyItem `json:"idempotency"`
}

type IdempotencyItem struct {
	UserID       string `json:"user_id"`
	Key          string `json:"key"`
	Method       string `json:"method"`
	Path         string `json:"path"`
	BodyHash     string `json:"body_hash"`
	StatusCode   int    `json:"status_code"`
	ResponseBody string `json:"response_body"`
}
