package event

import (
	"errors"
	"time"
)

var (
	ErrNotFound               = errors.New("event not found")
	ErrChatNotFound           = errors.New("chat not found")
	ErrInvalid                = errors.New("invalid event")
	ErrCapacityBelowConfirmed = errors.New("capacity below confirmed count")
	// ErrAlreadyCancelled reports that the game was called off earlier: the
	// caller tells the administrator instead of cancelling twice.
	ErrAlreadyCancelled = errors.New("event already cancelled")
)

// Status is an event lifecycle state. Values must match the CHECK constraint
// on events.status in the database.
type Status string

const (
	// StatusScheduled is a game people can sign up for.
	StatusScheduled Status = "scheduled"
	// StatusCancelled is a game the administrator called off. It stays in the
	// database — its bookings and its announcement point at it — but it takes
	// no more bookings and is not listed as upcoming.
	StatusCancelled Status = "cancelled"
)

type Event struct {
	ID        int64
	ChatID    int64
	StartsAt  time.Time
	Title     string
	Location  string
	Capacity  int
	Status    Status
	CreatedAt time.Time
}

// EventSummary is an Event together with its booking counters. Free is the
// number of remaining confirmed slots (capacity - confirmed) and is never
// negative.
type EventSummary struct {
	Event
	Confirmed int
	Waitlist  int
	Free      int
}

type CreateInput struct {
	ChatID   int64
	StartsAt time.Time
	Title    string
	Location string
	Capacity int
}
