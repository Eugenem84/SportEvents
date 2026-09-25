package event

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const defaultUpcomingLimit = 20

type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

func (s *Service) Create(ctx context.Context, in CreateInput) (Event, error) {
	if err := validateCreate(in); err != nil {
		return Event{}, err
	}

	const q = `
		INSERT INTO events (chat_id, starts_at, title, location, capacity)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, chat_id, starts_at, title, location, capacity, status, created_at`

	var e Event
	err := s.pool.QueryRow(ctx, q,
		in.ChatID,
		in.StartsAt.UTC(),
		in.Title,
		in.Location,
		in.Capacity,
	).Scan(&e.ID, &e.ChatID, &e.StartsAt, &e.Title, &e.Location, &e.Capacity, &e.Status, &e.CreatedAt)
	if isForeignKey(err) {
		return Event{}, ErrChatNotFound
	}
	if err != nil {
		return Event{}, fmt.Errorf("create event: %w", err)
	}
	return e, nil
}

func (s *Service) Get(ctx context.Context, chatID, eventID int64) (Event, error) {
	const q = `
		SELECT id, chat_id, starts_at, title, location, capacity, status, created_at
		FROM events
		WHERE id = $1 AND chat_id = $2`

	var e Event
	err := s.pool.QueryRow(ctx, q, eventID, chatID).
		Scan(&e.ID, &e.ChatID, &e.StartsAt, &e.Title, &e.Location, &e.Capacity, &e.Status, &e.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Event{}, ErrNotFound
	}
	if err != nil {
		return Event{}, fmt.Errorf("get event: %w", err)
	}
	return e, nil
}

// Cancel calls a game off. The event keeps its bookings and its announcement in
// the chat — people must still see what happened — but it takes no more
// bookings and disappears from the upcoming lists. Calling off an already
// cancelled game reports ErrAlreadyCancelled together with the event, so the
// caller can say «уже отменена» instead of pretending it worked.
func (s *Service) Cancel(ctx context.Context, chatID, eventID int64) (Event, error) {
	const q = `
		UPDATE events
		SET status = 'cancelled'
		WHERE id = $1 AND chat_id = $2 AND status = 'scheduled'
		RETURNING id, chat_id, starts_at, title, location, capacity, status, created_at`

	var e Event
	err := s.pool.QueryRow(ctx, q, eventID, chatID).
		Scan(&e.ID, &e.ChatID, &e.StartsAt, &e.Title, &e.Location, &e.Capacity, &e.Status, &e.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// Ничего не обновилось: либо такой игры нет, либо она уже отменена.
		existing, getErr := s.Get(ctx, chatID, eventID)
		if getErr != nil {
			return Event{}, getErr
		}
		return existing, ErrAlreadyCancelled
	}
	if err != nil {
		return Event{}, fmt.Errorf("cancel event: %w", err)
	}
	return e, nil
}

func (s *Service) ListUpcoming(ctx context.Context, chatID int64, from time.Time, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = defaultUpcomingLimit
	}

	const q = `
		SELECT id, chat_id, starts_at, title, location, capacity, status, created_at
		FROM events
		WHERE chat_id = $1 AND starts_at >= $2 AND status = 'scheduled'
		ORDER BY starts_at ASC, id ASC
		LIMIT $3`

	rows, err := s.pool.Query(ctx, q, chatID, from.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("list upcoming events: %w", err)
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.ChatID, &e.StartsAt, &e.Title, &e.Location, &e.Capacity, &e.Status, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list upcoming events: %w", err)
	}
	if out == nil {
		out = []Event{}
	}
	return out, nil
}

// ListUpcomingWithCounts returns the upcoming events of a chat together with
// their booking counters. Free is computed as capacity - confirmed, the way
// the chat renders "свободных мест"; free slots are never stored.
//
// One query, one row per event: the chat may render a dozen events at once,
// so counting per event would be N+1. Called-off games are not upcoming.
func (s *Service) ListUpcomingWithCounts(ctx context.Context, chatID int64, from time.Time, limit int) ([]EventSummary, error) {
	if limit <= 0 {
		limit = defaultUpcomingLimit
	}

	const q = `
		SELECT e.id, e.chat_id, e.starts_at, e.title, e.location, e.capacity, e.status, e.created_at,
		       count(b.id) FILTER (WHERE b.status = 'confirmed')::int AS confirmed,
		       count(b.id) FILTER (WHERE b.status = 'waitlist')::int  AS waitlist
		FROM events e
		LEFT JOIN bookings b ON b.event_id = e.id
		WHERE e.chat_id = $1 AND e.starts_at >= $2 AND e.status = 'scheduled'
		GROUP BY e.id
		ORDER BY e.starts_at ASC, e.id ASC
		LIMIT $3`

	rows, err := s.pool.Query(ctx, q, chatID, from.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("list upcoming events with counts: %w", err)
	}
	defer rows.Close()

	var out []EventSummary
	for rows.Next() {
		var e EventSummary
		if err := rows.Scan(
			&e.ID, &e.ChatID, &e.StartsAt, &e.Title, &e.Location, &e.Capacity, &e.Status, &e.CreatedAt,
			&e.Confirmed, &e.Waitlist,
		); err != nil {
			return nil, fmt.Errorf("scan event summary: %w", err)
		}
		e.Free = e.Capacity - e.Confirmed
		if e.Free < 0 {
			e.Free = 0
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list upcoming events with counts: %w", err)
	}
	if out == nil {
		out = []EventSummary{}
	}
	return out, nil
}

func (s *Service) SetCapacity(ctx context.Context, chatID, eventID int64, capacity int) error {
	// The narrow form of Update, kept for the bot, which changes only the
	// capacity (the rest of the announcement stays as it was).
	ev, err := s.Get(ctx, chatID, eventID)
	if err != nil {
		return err
	}
	_, err = s.Update(ctx, UpdateInput{
		ChatID:   chatID,
		EventID:  eventID,
		Title:    ev.Title,
		Location: ev.Location,
		StartsAt: ev.StartsAt,
		Capacity: capacity,
	})
	return err
}

func validateCreate(in CreateInput) error {
	if in.ChatID == 0 {
		return ErrInvalid
	}
	if in.StartsAt.IsZero() {
		return ErrInvalid
	}
	if strings.TrimSpace(in.Title) == "" {
		return ErrInvalid
	}
	if in.Capacity < 1 {
		return ErrInvalid
	}
	return nil
}

// Update applies an administrator's changes to a game and returns the result as
// it is stored now: the announcement in the chat is rewritten from it. The
// capacity may not drop below the confirmed lineup — those people already have
// seats (ARCHITECTURE §9) — and a called-off game is not editable: its
// announcement no longer offers anything.
func (s *Service) Update(ctx context.Context, in UpdateInput) (Event, error) {
	if in.ChatID == 0 || in.EventID == 0 {
		return Event{}, ErrInvalid
	}
	if in.StartsAt.IsZero() {
		return Event{}, ErrInvalid
	}
	if strings.TrimSpace(in.Title) == "" {
		return Event{}, ErrInvalid
	}
	if in.Capacity < 1 {
		return Event{}, ErrInvalid
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Event{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status Status
	err = tx.QueryRow(ctx,
		`SELECT status FROM events WHERE id = $1 AND chat_id = $2 FOR UPDATE`,
		in.EventID, in.ChatID,
	).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return Event{}, ErrNotFound
	}
	if err != nil {
		return Event{}, fmt.Errorf("lock event: %w", err)
	}
	if status == StatusCancelled {
		return Event{}, ErrAlreadyCancelled
	}

	var confirmed int
	err = tx.QueryRow(ctx,
		`SELECT count(*)::int FROM bookings WHERE event_id = $1 AND status = 'confirmed'`,
		in.EventID,
	).Scan(&confirmed)
	if err != nil {
		return Event{}, fmt.Errorf("count confirmed: %w", err)
	}
	if in.Capacity < confirmed {
		return Event{}, ErrCapacityBelowConfirmed
	}

	var e Event
	err = tx.QueryRow(ctx,
		`UPDATE events SET title = $1, location = $2, starts_at = $3, capacity = $4
		 WHERE id = $5 AND chat_id = $6
		 RETURNING id, chat_id, starts_at, title, location, capacity, status, created_at`,
		in.Title, in.Location, in.StartsAt, in.Capacity, in.EventID, in.ChatID,
	).Scan(&e.ID, &e.ChatID, &e.StartsAt, &e.Title, &e.Location, &e.Capacity, &e.Status, &e.CreatedAt)
	if err != nil {
		return Event{}, fmt.Errorf("update event: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Event{}, fmt.Errorf("commit: %w", err)
	}
	return e, nil
}

func isForeignKey(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}
