// Package schedule stores the regular game times of a chat: which weekday and
// what time games usually start. Creating a game then needs no date at all —
// the adapter takes the nearest slot.
package schedule

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	// ErrInvalid reports a malformed slot (unknown weekday, bad time).
	ErrInvalid = errors.New("invalid schedule slot")
	// ErrChatNotFound reports a slot for a chat that does not exist.
	ErrChatNotFound = errors.New("chat not found")
)

// Slot is one regular game time: a weekday and a time of day, both in the
// chat's timezone.
type Slot struct {
	ChatID int64
	// Weekday follows time.Weekday and PostgreSQL DOW: 0 = воскресенье.
	Weekday int
	// Minutes is the time of day as minutes since midnight (0..1439).
	Minutes int
}

// At renders the slot time the way the chat writes it: "10:00".
func (s Slot) At() string { return FormatMinutes(s.Minutes) }

// FormatMinutes renders minutes since midnight as "HH:MM".
func FormatMinutes(minutes int) string {
	return fmt.Sprintf("%02d:%02d", minutes/60, minutes%60)
}

// ParseTime reads "10:00" (and "10.00", "9:30") into minutes since midnight.
func ParseTime(text string) (int, bool) {
	hour, minute, ok := parseClock(text)
	if !ok {
		return 0, false
	}
	return hour*60 + minute, true
}

// Service stores chat schedules in PostgreSQL.
type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// List returns the slots of a chat, ordered by weekday.
func (s *Service) List(ctx context.Context, chatID int64) ([]Slot, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT chat_id, weekday, minutes FROM chat_game_slots WHERE chat_id = $1 ORDER BY weekday`,
		chatID,
	)
	if err != nil {
		return nil, fmt.Errorf("list schedule: %w", err)
	}
	defer rows.Close()

	var out []Slot
	for rows.Next() {
		var slot Slot
		if err := rows.Scan(&slot.ChatID, &slot.Weekday, &slot.Minutes); err != nil {
			return nil, fmt.Errorf("scan schedule slot: %w", err)
		}
		out = append(out, slot)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list schedule: %w", err)
	}
	if out == nil {
		out = []Slot{}
	}
	return out, nil
}

// Set adds a weekday to the schedule or replaces its time: in V1 one weekday
// carries exactly one time.
func (s *Service) Set(ctx context.Context, chatID int64, weekday, minutes int) (Slot, error) {
	if err := validate(chatID, weekday, minutes); err != nil {
		return Slot{}, err
	}

	slot := Slot{ChatID: chatID, Weekday: weekday, Minutes: minutes}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO chat_game_slots (chat_id, weekday, minutes)
		VALUES ($1, $2, $3)
		ON CONFLICT (chat_id, weekday) DO UPDATE
		SET minutes = EXCLUDED.minutes,
		    updated_at = now()
		RETURNING chat_id, weekday, minutes`,
		chatID, weekday, minutes,
	).Scan(&slot.ChatID, &slot.Weekday, &slot.Minutes)
	if isForeignKey(err) {
		return Slot{}, ErrChatNotFound
	}
	if err != nil {
		return Slot{}, fmt.Errorf("set schedule slot: %w", err)
	}
	return slot, nil
}

// Delete removes a weekday from the schedule. ok is false when that weekday
// was not in the schedule — the adapter reports it as "и так не настроено".
func (s *Service) Delete(ctx context.Context, chatID int64, weekday int) (bool, error) {
	if chatID == 0 || weekday < 0 || weekday > 6 {
		return false, ErrInvalid
	}

	tag, err := s.pool.Exec(ctx,
		`DELETE FROM chat_game_slots WHERE chat_id = $1 AND weekday = $2`,
		chatID, weekday,
	)
	if err != nil {
		return false, fmt.Errorf("delete schedule slot: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

func validate(chatID int64, weekday, minutes int) error {
	if chatID == 0 {
		return ErrInvalid
	}
	if weekday < 0 || weekday > 6 {
		return ErrInvalid
	}
	if minutes < 0 || minutes > 24*60-1 {
		return ErrInvalid
	}
	return nil
}

func isForeignKey(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}
