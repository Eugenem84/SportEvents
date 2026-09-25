package booking

import (
	"errors"
	"time"
)

var (
	ErrInvalid       = errors.New("invalid booking")
	ErrEventNotFound = errors.New("event not found")
	ErrUserNotFound  = errors.New("user not found")
	ErrNotFound      = errors.New("booking not found")
	ErrAlreadyBooked = errors.New("user already has an active booking for this event")
	ErrNotActive     = errors.New("booking is not active")
)

// Status is a booking lifecycle state. Values must match the CHECK
// constraint on bookings.status in the database.
type Status string

const (
	StatusConfirmed Status = "confirmed"
	StatusWaitlist  Status = "waitlist"
	StatusCancelled Status = "cancelled"
)

// Booking is a single person's participation record for an Event.
// UserID is nil for a guest without a messenger identity.
type Booking struct {
	ID             int64
	EventID        int64
	PlayerName     string
	Phone          *string
	UserID         *int64
	BookedByUserID int64
	Status         Status
	CreatedAt      time.Time
}

// CreateInput describes a new booking request. UserID is nil for a guest
// added by another participant or by an administrator.
type CreateInput struct {
	EventID        int64
	PlayerName     string
	Phone          string
	UserID         *int64
	BookedByUserID int64
}

// BookingWithEvent is a booking together with the event it belongs to. It is
// what "мои записи" renders: status, when the game is, and the booking id to
// cancel.
type BookingWithEvent struct {
	Booking
	EventTitle    string
	EventStartsAt time.Time
	EventLocation string
	EventCapacity int
}

// CancelResult is the outcome of cancelling a booking. Promoted is set
// when cancelling freed a confirmed slot and the first waitlist booking
// was moved to confirmed. Callers (e.g. the VK adapter) use Promoted to
// decide whom to notify: Promoted.UserID for an identified user, or
// Promoted.BookedByUserID / the chat otherwise.
type CancelResult struct {
	Cancelled Booking
	Promoted  *Booking
}
