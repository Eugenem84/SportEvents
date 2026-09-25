// Package announce remembers where an event was announced in an external
// chat, so the roster of participants can be updated in the same message
// instead of posting a new message on every booking.
package announce

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrInvalid reports a malformed Ref.
var ErrInvalid = errors.New("invalid announcement ref")

// Ref points at the message that announces an event in an external chat.
type Ref struct {
	EventID        int64
	Platform       string
	ExternalChatID string
	MessageID      int64
}

// Service stores announcement references in PostgreSQL.
type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// Save remembers the announcement message of an event. The last message wins:
// announcing the same event again replaces the reference.
func (s *Service) Save(ctx context.Context, ref Ref) error {
	if ref.EventID == 0 || strings.TrimSpace(ref.Platform) == "" ||
		strings.TrimSpace(ref.ExternalChatID) == "" || ref.MessageID == 0 {
		return ErrInvalid
	}

	_, err := s.pool.Exec(ctx, `
		INSERT INTO event_announcements (event_id, platform, external_chat_id, message_id)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (event_id, platform) DO UPDATE
		SET external_chat_id = EXCLUDED.external_chat_id,
		    message_id       = EXCLUDED.message_id,
		    updated_at       = now()`,
		ref.EventID, ref.Platform, ref.ExternalChatID, ref.MessageID,
	)
	if err != nil {
		return fmt.Errorf("save announcement: %w", err)
	}
	return nil
}

// Get returns the announcement of an event for a platform. ok is false when
// the event has never been announced there.
func (s *Service) Get(ctx context.Context, eventID int64, platform string) (Ref, bool, error) {
	var ref Ref
	err := s.pool.QueryRow(ctx, `
		SELECT event_id, platform, external_chat_id, message_id
		FROM event_announcements
		WHERE event_id = $1 AND platform = $2`,
		eventID, platform,
	).Scan(&ref.EventID, &ref.Platform, &ref.ExternalChatID, &ref.MessageID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Ref{}, false, nil
	}
	if err != nil {
		return Ref{}, false, fmt.Errorf("get announcement: %w", err)
	}
	return ref, true, nil
}
