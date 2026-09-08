package chat

import (
	"errors"
	"time"
)

// V1 supports a single platform; it is kept as a value so a later
// platform can be added without relocating the external id.
const PlatformVK = "vk"

var ErrChatNotFound = errors.New("chat not found")

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
