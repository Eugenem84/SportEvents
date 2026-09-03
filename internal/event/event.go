package event

import (
	"errors"
	"time"
)

var (
	ErrNotFound                = errors.New("event not found")
	ErrChatNotFound            = errors.New("chat not found")
	ErrInvalid                 = errors.New("invalid event")
	ErrCapacityBelowConfirmed  = errors.New("capacity below confirmed count")
)

type Event struct {
	ID        int64
	ChatID    int64
	StartsAt  time.Time
	Title     string
	Location  string
	Capacity  int
	CreatedAt time.Time
}

type CreateInput struct {
	ChatID   int64
	StartsAt time.Time
	Title    string
	Location string
	Capacity int
}
