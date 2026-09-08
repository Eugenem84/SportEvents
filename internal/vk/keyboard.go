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
	raw, err := json.Marshal(struct {
		Command string `json:"command"`
	}{command})
	if err != nil {
		return "{}"
	}
	return string(raw)
}

// PayloadCommand reads the command out of a VK payload string. An empty or
// unparseable payload yields an empty command.
func PayloadCommand(payload string) string {
	var p struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return ""
	}
	return p.Command
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
