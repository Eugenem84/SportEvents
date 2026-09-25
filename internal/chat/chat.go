package chat

import (
	"errors"
	"time"
)

// V1 supports a single platform; it is kept as a value so a later
// platform can be added without relocating the external id.
const PlatformVK = "vk"

var (
	ErrChatNotFound = errors.New("chat not found")
	// ErrInvalidConnect reports a malformed Connect input (missing
	// platform, external id, title or initiator).
	ErrInvalidConnect = errors.New("invalid chat connect input")
	// ErrNotAdmin reports that the person does not administer the chat: only an
	// administrator may change the games of a chat, from the bot or from the
	// mini app.
	ErrNotAdmin = errors.New("user is not a chat admin")
)

type Chat struct {
	ID        int64
	Title     string
	CreatedAt time.Time
}

type User struct {
	ID          int64
	DisplayName string
	CreatedAt   time.Time
}

// ConnectInput registers an external conversation as an internal Chat,
// making the initiating participant its first administrator.
type ConnectInput struct {
	Platform       string
	ExternalChatID string
	ChatTitle      string
	InitiatorID    int64
}
