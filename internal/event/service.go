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
		RETURNING id, chat_id, starts_at, title, location, capacity, created_at`

	var e Event
	err := s.pool.QueryRow(ctx, q,
		in.ChatID,
		in.StartsAt.UTC(),
		in.Title,
		in.Location,
		in.Capacity,
	).Scan(&e.ID, &e.ChatID, &e.StartsAt, &e.Title, &e.Location, &e.Capacity, &e.CreatedAt)
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
		SELECT id, chat_id, starts_at, title, location, capacity, created_at
		FROM events
		WHERE id = $1 AND chat_id = $2`

	var e Event
	err := s.pool.QueryRow(ctx, q, eventID, chatID).
		Scan(&e.ID, &e.ChatID, &e.StartsAt, &e.Title, &e.Location, &e.Capacity, &e.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Event{}, ErrNotFound
	}
	if err != nil {
		return Event{}, fmt.Errorf("get event: %w", err)
	}
	return e, nil
}

func (s *Service) ListUpcoming(ctx context.Context, chatID int64, from time.Time, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = defaultUpcomingLimit
	}

	const q = `
		SELECT id, chat_id, starts_at, title, location, capacity, created_at
		FROM events
		WHERE chat_id = $1 AND starts_at >= $2
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
		if err := rows.Scan(&e.ID, &e.ChatID, &e.StartsAt, &e.Title, &e.Location, &e.Capacity, &e.CreatedAt); err != nil {
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

func (s *Service) SetCapacity(ctx context.Context, chatID, eventID int64, capacity int) error {
	if capacity < 1 {
		return ErrInvalid
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var lockedID int64
	err = tx.QueryRow(ctx,
		`SELECT id FROM events WHERE id = $1 AND chat_id = $2 FOR UPDATE`,
		eventID, chatID,
	).Scan(&lockedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lock event: %w", err)
	}

	var confirmed int
	err = tx.QueryRow(ctx,
		`SELECT count(*)::int FROM bookings WHERE event_id = $1 AND status = 'confirmed'`,
		eventID,
	).Scan(&confirmed)
	if err != nil {
		return fmt.Errorf("count confirmed: %w", err)
	}
	if capacity < confirmed {
		return ErrCapacityBelowConfirmed
	}

	_, err = tx.Exec(ctx, `UPDATE events SET capacity = $1 WHERE id = $2`, capacity, eventID)
	if err != nil {
		return fmt.Errorf("update capacity: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
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

func isForeignKey(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}
