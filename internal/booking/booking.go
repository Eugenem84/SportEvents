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
	// ErrSeatTaken reports that the seat the caller asked for is already held by
	// a confirmed booking: in a chat people write «3» meaning seat 3, and the
	// bot answers who has it.
	ErrSeatTaken = errors.New("seat is already taken")
	// ErrSeatOutOfRange reports a seat outside 1..capacity.
	ErrSeatOutOfRange = errors.New("seat is out of the game's range")
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
// UserID is nil for a guest without a messenger identity. SeatNo is the seat
// in the lineup: it is assigned when the booking is confirmed, freed on
// cancellation and taken by the next confirmed booking, so the numbering in
// the announcement never shifts.
type Booking struct {
	ID             int64
	EventID        int64
	PlayerName     string
	Phone          *string
	UserID         *int64
	BookedByUserID int64
	SeatNo         *int
	Status         Status
	CreatedAt      time.Time
}

// CreateInput describes a new booking request. UserID is nil for a guest
// added by another participant or by an administrator. SeatNo is the seat the
// caller asked for (in the chat people write «11 Сергей Иванов»); zero means
// «any free seat», and the domain takes the smallest one.
type CreateInput struct {
	EventID        int64
	PlayerName     string
	Phone          string
	UserID         *int64
	BookedByUserID int64
	SeatNo         int
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
