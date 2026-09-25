package schedule

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Weekday names live next to the parsers: the bot writes them into the chat
// and reads them back out of the admin's messages.
var (
	shortWeekdayNames = [7]string{"вс", "пн", "вт", "ср", "чт", "пт", "сб"}
	longWeekdayNames  = [7]string{"воскресенье", "понедельник", "вторник", "среда", "четверг", "пятница", "суббота"}
)

// weekdayWords maps everything people plausibly type to a weekday. The
// accusative forms are there because "добавить игру в среду" is natural
// Russian, not a typo.
var weekdayWords = map[string]int{
	"вс": 0, "вскр": 0, "воскресенье": 0, "воскресение": 0,
	"пн": 1, "пон": 1, "понедельник": 1,
	"вт": 2, "вторник": 2,
	"ср": 3, "сред": 3, "среда": 3, "среду": 3,
	"чт": 4, "четверг": 4,
	"пт": 5, "пятн": 5, "пятница": 5, "пятницу": 5,
	"сб": 6, "суб": 6, "суббота": 6, "субботу": 6,
}

// ShortWeekday returns the two-letter name the bot prints ("вс").
func ShortWeekday(weekday int) string {
	if weekday < 0 || weekday > 6 {
		return ""
	}
	return shortWeekdayNames[weekday]
}

// LongWeekday returns the full name ("воскресенье").
func LongWeekday(weekday int) string {
	if weekday < 0 || weekday > 6 {
		return ""
	}
	return longWeekdayNames[weekday]
}

// ParseWeekday recognises a weekday word, with or without a trailing dot.
func ParseWeekday(word string) (int, bool) {
	w := strings.Trim(strings.ToLower(strings.TrimSpace(word)), ".,")
	day, ok := weekdayWords[w]
	return day, ok
}

// Describe renders a schedule the way the chat reads it: "вс 10:00, ср 19:00".
// An empty schedule becomes "—".
func Describe(slots []Slot) string {
	if len(slots) == 0 {
		return "—"
	}
	parts := make([]string, 0, len(slots))
	for _, slot := range slots {
		parts = append(parts, fmt.Sprintf("%s %s", ShortWeekday(slot.Weekday), slot.At()))
	}
	return strings.Join(parts, ", ")
}

var clockRE = regexp.MustCompile(`\b([01]?\d|2[0-3])[:.](\d{2})\b`)

// parseClock pulls the first "HH:MM" out of a text. The hour is bounded by
// the pattern; the minutes are checked here, so "10:75" is not a time.
func parseClock(text string) (hour, minute int, ok bool) {
	m := clockRE.FindStringSubmatch(text)
	if m == nil {
		return 0, 0, false
	}
	hour, _ = strconv.Atoi(m[1])
	minute, _ = strconv.Atoi(m[2])
	if minute > 59 {
		return 0, 0, false
	}
	return hour, minute, true
}
