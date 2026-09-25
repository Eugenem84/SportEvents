package vk

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	eventConfirmation = "confirmation"
	eventMessageNew   = "message_new"
	eventMessageEvent = "message_event"
)

// CallbackRequest is the envelope VK POSTs to the callback server for every
// enabled event.
type CallbackRequest struct {
	Type    string          `json:"type"`
	GroupID int64           `json:"group_id"`
	Secret  string          `json:"secret"`
	EventID string          `json:"event_id"`
	Object  json.RawMessage `json:"object"`
}

// payloadField is VK's "payload" field. The Callback API docs describe it as a
// JSON string, but Bots Long Poll sends an already decoded object, so both
// forms must parse: a string is used as is, an object is kept as its JSON text
// for ParseButtonPayload.
type payloadField string

func (p *payloadField) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	switch {
	case trimmed == "" || trimmed == "null":
		*p = ""
	case strings.HasPrefix(trimmed, `"`):
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		*p = payloadField(s)
	default:
		*p = payloadField(trimmed)
	}
	return nil
}

// String returns the payload as JSON text.
func (p payloadField) String() string { return string(p) }

// messageNew is the object.message of a message_new event.
type messageNew struct {
	ID     int64  `json:"id"`
	Date   int64  `json:"date"`
	PeerID int64  `json:"peer_id"`
	FromID int64  `json:"from_id"`
	Text   string `json:"text"`
	Out    int    `json:"out"`
	// Payload is attached when the message was sent by pressing a button
	// of the bot keyboard.
	Payload payloadField `json:"payload"`
}

type messageNewObject struct {
	Message messageNew `json:"message"`
}

// messageEvent is the object of a message_event (callback button press).
// ConversationMessageID is the id of the message whose button was pressed:
// the bot edits that very message (the settings screen), so the chat keeps
// one live settings message instead of a pile of them.
type messageEvent struct {
	UserID                int64        `json:"user_id"`
	PeerID                int64        `json:"peer_id"`
	EventID               string       `json:"event_id"`
	Payload               payloadField `json:"payload"`
	ConversationMessageID int64        `json:"conversation_message_id"`
}

// HandleCallback is the POST /vk/callback endpoint. VK retries events that
// receive a non-200 answer, so failures must be logged, not propagated.
func (s *Service) HandleCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		s.log.Printf("vk: read body: %v", err)
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}

	var cb CallbackRequest
	if err := json.Unmarshal(body, &cb); err != nil {
		s.log.Printf("vk: invalid json: %v", err)
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	if cb.Type == eventConfirmation {
		s.log.Printf("vk: confirmation request")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, s.confirmationToken)
		return
	}

	if err := s.authorize(cb); err != nil {
		s.log.Printf("vk: rejected callback: %v", err)
		http.Error(w, "unauthorized", http.StatusForbidden)
		return
	}

	// VK retries events it did not get a confident answer for. The same
	// event_id must be processed once: a retry is logged and skipped.
	if cb.EventID != "" && s.dedup.mark(cb.EventID) {
		s.log.Printf("vk: duplicate event %q (%s), skipping", cb.EventID, cb.Type)
	} else {
		s.dispatch(r.Context(), cb)
	}

	// VK stops delivering events after repeated failures, so always answer
	// ok for non-confirmation events even when we ignored them.
	s.answerOK(w)
}

// dispatch routes one callback event to the matching handler.
func (s *Service) dispatch(ctx context.Context, cb CallbackRequest) {
	switch cb.Type {
	case eventMessageNew:
		var obj messageNewObject
		if err := json.Unmarshal(cb.Object, &obj); err != nil {
			s.log.Printf("vk: bad message_new: %v", err)
			return
		}
		if err := s.handleMessageNew(ctx, obj.Message); err != nil {
			s.log.Printf("vk: message_new: %v", err)
		}
	case eventMessageEvent:
		var ev messageEvent
		if err := json.Unmarshal(cb.Object, &ev); err != nil {
			s.log.Printf("vk: bad message_event: %v", err)
			return
		}
		if err := s.handleMessageEvent(ctx, ev); err != nil {
			s.log.Printf("vk: message_event: %v", err)
		}
	default:
		s.log.Printf("vk: unhandled event type %q", cb.Type)
	}
}

func (s *Service) answerOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "ok")
}

// authorize rejects events from another secret or another community.
// The confirmation event must not be authorized: it has no secret.
func (s *Service) authorize(cb CallbackRequest) error {
	if s.secret != "" {
		if subtle.ConstantTimeCompare([]byte(cb.Secret), []byte(s.secret)) != 1 {
			return fmt.Errorf("secret mismatch")
		}
	}
	if s.groupID != 0 && cb.GroupID != 0 && cb.GroupID != s.groupID {
		return fmt.Errorf("group_id mismatch: got %d want %d", cb.GroupID, s.groupID)
	}
	return nil
}
