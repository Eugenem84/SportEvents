package chat

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Service resolves internal Chat and User from external platform ids.
// It never calls the messenger: mapping external ids to internal entities
// is adapter work; this package only maps them to/from PostgreSQL.
type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// FindByChannel returns the Chat bound to the external channel
// (platform, externalChatID). It returns (nil, nil) when the channel is not
// connected to any Chat yet — the message came from an unregistered
// conversation.
func (s *Service) FindByChannel(ctx context.Context, platform, externalChatID string) (*Chat, error) {
	const q = `
		SELECT c.id, c.title, c.created_at
		FROM chat_channels cc
		JOIN chats c ON c.id = cc.chat_id
		WHERE cc.platform = $1 AND cc.external_chat_id = $2`

	var c Chat
	err := s.pool.QueryRow(ctx, q, platform, externalChatID).
		Scan(&c.ID, &c.Title, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find chat by channel: %w", err)
	}
	return &c, nil
}

// FindOrCreateUserByExternalID returns the internal User for the external
// identity, creating both rows on first contact. Repeated calls with the
// same ids return the same User: the (platform, external_user_id) unique
// index plus the conflict handling make duplicate callbacks idempotent.
func (s *Service) FindOrCreateUserByExternalID(ctx context.Context, platform, externalUserID, displayName string) (User, error) {
	u, ok, err := s.findUserByIdentity(ctx, platform, externalUserID)
	if err != nil {
		return User{}, err
	}
	if ok {
		return u, nil
	}

	// First contact: create the user and the identity row. Two concurrent
	// callbacks (retries of the same VK event) may both reach this point;
	// only one wins the unique index, the loser must not keep its own user
	// row — it rolls back the transaction and returns the winner's user.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	err = tx.QueryRow(ctx,
		`INSERT INTO users (display_name) VALUES ($1)
		 RETURNING id, display_name, created_at`,
		displayName,
	).Scan(&u.ID, &u.DisplayName, &u.CreatedAt)
	if err != nil {
		return User{}, fmt.Errorf("insert user: %w", err)
	}

	// ON CONFLICT DO NOTHING RETURNING reports who actually owns the
	// identity: the caller. When another concurrent request inserted it
	// first, no row comes back; we discard our fresh (and now orphan)
	// user row via the deferred rollback and re-read the existing user.
	var identityUserID int64
	err = tx.QueryRow(ctx,
		`INSERT INTO user_identities (user_id, platform, external_user_id)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (platform, external_user_id) DO NOTHING
		 RETURNING user_id`,
		u.ID, platform, externalUserID,
	).Scan(&identityUserID)
	if errors.Is(err, pgx.ErrNoRows) {
		u2, _, err := s.findUserByIdentity(ctx, platform, externalUserID)
		return u2, err
	}
	if err != nil {
		return User{}, fmt.Errorf("insert identity: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return User{}, fmt.Errorf("commit: %w", err)
	}
	return u, nil
}

func (s *Service) findUserByIdentity(ctx context.Context, platform, externalUserID string) (User, bool, error) {
	const q = `
		SELECT u.id, u.display_name, u.created_at
		FROM user_identities ui
		JOIN users u ON u.id = ui.user_id
		WHERE ui.platform = $1 AND ui.external_user_id = $2`

	var u User
	err := s.pool.QueryRow(ctx, q, platform, externalUserID).
		Scan(&u.ID, &u.DisplayName, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, false, nil
	}
	if err != nil {
		return User{}, false, fmt.Errorf("find user by identity: %w", err)
	}
	return u, true, nil
}
