package vk

import (
	"encoding/json"
	"fmt"
)

// Keyboard and Button mirror the VK Bot API "keyboard" parameter of
// messages.send. Only text/callback style buttons are needed for V1.
type ButtonColor string

const (
	ColorPrimary   ButtonColor = "primary"
	ColorSecondary ButtonColor = "secondary"
	ColorNegative  ButtonColor = "negative"
	ColorPositive  ButtonColor = "positive"
)

type Keyboard struct {
	OneTime bool       `json:"one_time,omitempty"`
	Inline  bool       `json:"inline,omitempty"`
	Buttons [][]Button `json:"buttons"`
}

type Button struct {
	Action ButtonAction `json:"action"`
	Color  ButtonColor  `json:"color,omitempty"`
}

type ButtonAction struct {
	Type    string `json:"type"`
	Label   string `json:"label,omitempty"`
	Payload string `json:"payload,omitempty"`
}

// TextButton builds a keyboard button that sends its label as a chat
// message plus the JSON payload. VK delivers a pressed text button as a
// message_new event with the payload attached.
func TextButton(label, payload string, color ButtonColor) Button {
	return Button{
		Action: ButtonAction{Type: "text", Label: label, Payload: payload},
		Color:  color,
	}
}

// CommandPayload wraps a command name into the JSON payload VK forwards
// with the button press.
func CommandPayload(command string) string {
	return EventCommandPayload(command, 0)
}

// EventCommandPayload is the payload of a button that acts on a specific
// game: the command plus the internal event id. The label of such a button
// carries no state, so the same "Записаться" can appear under every game.
func EventCommandPayload(command string, eventID int64) string {
	return Payload{Command: command, EventID: eventID}.Marshal()
}

// SlotCommandPayload is the payload of a settings button: the command plus
// the weekday it applies to. The weekday is always written out (воскресенье
// is 0), so a payload cannot lose it.
func SlotCommandPayload(command string, weekday int) string {
	raw, err := json.Marshal(struct {
		Command string `json:"command"`
		Weekday int    `json:"weekday"`
	}{command, weekday})
	if err != nil {
		return "{}"
	}
	return string(raw)
}

// Payload is what a button carries: a command plus its parameters. VK hands
// it back as a string, both for text buttons (message_new) and for callback
// buttons (message_event).
type Payload struct {
	Command string `json:"command"`
	EventID int64  `json:"event_id,omitempty"`
	Weekday int    `json:"weekday,omitempty"`
}

// Marshal renders the payload for a button.
func (p Payload) Marshal() string {
	raw, err := json.Marshal(p)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

// PayloadCommand reads the command out of a VK payload string. An empty or
// unparseable payload yields an empty command.
func PayloadCommand(payload string) string {
	command, _ := ParsePayload(payload)
	return command
}

// ParsePayload reads the command and the optional event id out of a VK
// payload string.
func ParsePayload(payload string) (command string, eventID int64) {
	p := ParseButtonPayload(payload)
	return p.Command, p.EventID
}

// ParseButtonPayload decodes a payload; an empty or broken payload yields a
// zero Payload.
func ParseButtonPayload(payload string) Payload {
	var p Payload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return Payload{}
	}
	return p
}

// CallbackButton builds an inline button whose press VK delivers as
// message_event: nothing is posted to the chat, and VK tells us which message
// was pressed — that is how the settings screen rewrites itself in place.
// It requires the message_event event type to be enabled for the community.
func CallbackButton(label, payload string, color ButtonColor) Button {
	return Button{
		Action: ButtonAction{Type: "callback", Label: label, Payload: payload},
		Color:  color,
	}
}

// Marshal returns the keyboard as the JSON string VK expects in the
// messages.send "keyboard" parameter.
func (k Keyboard) Marshal() (string, error) {
	raw, err := json.Marshal(k)
	if err != nil {
		return "", fmt.Errorf("marshal keyboard: %w", err)
	}
	return string(raw), nil
}
