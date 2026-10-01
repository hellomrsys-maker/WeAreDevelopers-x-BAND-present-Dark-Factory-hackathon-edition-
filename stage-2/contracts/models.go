package contracts

import (
	"encoding/json"
	"time"
)

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
	Combinable                 [][]string    `json:"combinable,omitempty"`
}

// Reservation represents a booked or cancelled table assignment
type Reservation struct {
	ID            string    `json:"reservation_id"`
	Reference     string    `json:"reference"`
	RestaurantID  string    `json:"restaurant_id"`
	TableIDs      []string  `json:"table_ids"`
	TableID       *string   `json:"table_id,omitempty"`
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

// Custom UnmarshalJSON to handle both table_id and table_ids, and default confirmed status
func (r *Reservation) UnmarshalJSON(data []byte) error {
	type Alias Reservation
	aux := struct {
		RawTableID  *string   `json:"table_id"`
		RawTableIDs []string  `json:"table_ids"`
		*Alias
	}{
		Alias: (*Alias)(r),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}

	if len(aux.RawTableIDs) > 0 {
		r.TableIDs = aux.RawTableIDs
	} else if aux.RawTableID != nil && *aux.RawTableID != "" {
		r.TableIDs = []string{*aux.RawTableID}
	}

	if len(r.TableIDs) == 1 {
		r.TableID = &r.TableIDs[0]
	} else {
		r.TableID = nil
	}

	if r.Status == "" {
		r.Status = "confirmed"
	}
	return nil
}

// AvailabilityOption represents an available single table or combinable pair
type AvailabilityOption struct {
	TableIDs []string `json:"table_ids"`
	Capacity int      `json:"capacity"`
}

// AvailabilitySlot represents an available interval on the booking grid
type AvailabilitySlot struct {
	StartsAtLocal     string               `json:"starts_at_local"`
	StartsAt          string               `json:"starts_at"`
	AvailableTableIDs []string             `json:"available_table_ids"`
	AvailableOptions  []AvailabilityOption `json:"available_options"`
}

// ReservationMoveRequest defines a target change for an existing reservation
type ReservationMoveRequest struct {
	Reference     string   `json:"reference"`
	TableIDs      []string `json:"table_ids,omitempty"`
	TableID       *string  `json:"table_id,omitempty"`
	StartsAtLocal *string  `json:"starts_at_local,omitempty"`
	PartySize     *int     `json:"party_size,omitempty"`
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
