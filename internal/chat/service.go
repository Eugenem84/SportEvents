package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"

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

// IdentitiesByUsers maps internal user ids to their external ids on one
// platform. The mini app needs VK user ids to load avatars for the lineup:
// bookings keep the internal user, and only user_identities knows the VK id
// behind it. Users without an identity on that platform are absent from the
// map, and the app falls back to initials for them.
func (s *Service) IdentitiesByUsers(ctx context.Context, platform string, userIDs []int64) (map[int64]string, error) {
	out := make(map[int64]string, len(userIDs))
	if len(userIDs) == 0 {
		return out, nil
	}

	const q = `
		SELECT user_id, external_user_id
		FROM user_identities
		WHERE platform = $1 AND user_id = ANY($2)`

	rows, err := s.pool.Query(ctx, q, platform, userIDs)
	if err != nil {
		return nil, fmt.Errorf("identities by users: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var userID int64
		var externalID string
		if err := rows.Scan(&userID, &externalID); err != nil {
			return nil, fmt.Errorf("scan identity: %w", err)
		}
		out[userID] = externalID
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("identities by users: %w", err)
	}
	return out, nil
}

// Connect binds an external conversation to an internal Chat, creating the
// Chat, its ChatChannel and the first ChatAdmin in one transaction. It is
// idempotent: connecting an already connected conversation returns the
// existing Chat with created=false and never creates a second Chat.
//
// A concurrent connect (a retried VK callback) is serialized by the
// UNIQUE (platform, external_chat_id) index: the loser gets no row back
// from the channel insert, discards its own Chat via the deferred
// rollback and returns the winner's Chat.
func (s *Service) Connect(ctx context.Context, in ConnectInput) (Chat, bool, error) {
	if err := validateConnect(in); err != nil {
		return Chat{}, false, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Chat{}, false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var c Chat
	err = tx.QueryRow(ctx,
		`INSERT INTO chats (title) VALUES ($1)
		 RETURNING id, title, created_at`,
		in.ChatTitle,
	).Scan(&c.ID, &c.Title, &c.CreatedAt)
	if err != nil {
		return Chat{}, false, fmt.Errorf("insert chat: %w", err)
	}

	// ON CONFLICT DO NOTHING RETURNING reports who actually owns the
	// channel: the caller. When another request connected the conversation
	// first, no row comes back; the deferred rollback discards our fresh
	// (and now orphan) Chat row and we return the existing Chat.
	var channelID int64
	err = tx.QueryRow(ctx,
		`INSERT INTO chat_channels (chat_id, platform, external_chat_id)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (platform, external_chat_id) DO NOTHING
		 RETURNING id`,
		c.ID, in.Platform, in.ExternalChatID,
	).Scan(&channelID)
	if errors.Is(err, pgx.ErrNoRows) {
		existing, ferr := s.FindByChannel(ctx, in.Platform, in.ExternalChatID)
		if ferr != nil {
			return Chat{}, false, ferr
		}
		if existing == nil {
			return Chat{}, false, fmt.Errorf("connect: channel %s/%s vanished after conflict", in.Platform, in.ExternalChatID)
		}
		return *existing, false, nil
	}
	if err != nil {
		return Chat{}, false, fmt.Errorf("insert channel: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO chat_admins (chat_id, user_id) VALUES ($1, $2)
		 ON CONFLICT DO NOTHING`,
		c.ID, in.InitiatorID,
	); err != nil {
		return Chat{}, false, fmt.Errorf("insert admin: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Chat{}, false, fmt.Errorf("commit: %w", err)
	}
	return c, true, nil
}

// IsChatAdmin reports whether the user administers the Chat.
func (s *Service) IsChatAdmin(ctx context.Context, chatID, userID int64) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(
			SELECT 1 FROM chat_admins WHERE chat_id = $1 AND user_id = $2
		)`,
		chatID, userID,
	).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("is chat admin: %w", err)
	}
	return ok, nil
}

func validateConnect(in ConnectInput) error {
	if strings.TrimSpace(in.Platform) == "" {
		return ErrInvalidConnect
	}
	if strings.TrimSpace(in.ExternalChatID) == "" {
		return ErrInvalidConnect
	}
	if strings.TrimSpace(in.ChatTitle) == "" {
		return ErrInvalidConnect
	}
	if in.InitiatorID == 0 {
		return ErrInvalidConnect
	}
	return nil
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
