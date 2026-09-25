package schedule_test

import (
	"testing"
	"time"

	"sportevents.local/internal/schedule"
)

// The chat zone is fixed in V1 (ARCHITECTURE §7), so the tests use the same
// offset the adapter does.
var msk = time.FixedZone("MSK", 3*60*60)

func slot(weekday, hour, minute int) schedule.Slot {
	return schedule.Slot{Weekday: weekday, Minutes: hour*60 + minute}
}

func TestNextTodayBeforeTime(t *testing.T) {
	// Пятница 25.09.2026, 09:00 — игра в 10:00 ещё впереди.
	now := time.Date(2026, 9, 25, 9, 0, 0, 0, msk)
	got, ok := schedule.Next(now, []schedule.Slot{slot(5, 10, 0)}, msk)
	if !ok {
		t.Fatal("want a slot")
	}
	want := time.Date(2026, 9, 25, 10, 0, 0, 0, msk).UTC()
	if !got.Equal(want) {
		t.Fatalf("got %v want %v", got.In(msk), want.In(msk))
	}
}

func TestNextTodayAfterTimeGoesToNextWeek(t *testing.T) {
	// Пятница 25.09.2026, 11:00 — игра в 10:00 уже прошла.
	now := time.Date(2026, 9, 25, 11, 0, 0, 0, msk)
	got, ok := schedule.Next(now, []schedule.Slot{slot(5, 10, 0)}, msk)
	if !ok {
		t.Fatal("want a slot")
	}
	want := time.Date(2026, 10, 2, 10, 0, 0, 0, msk).UTC()
	if !got.Equal(want) {
		t.Fatalf("got %v want %v", got.In(msk), want.In(msk))
	}
}

func TestNextExactMomentIsStillFuture(t *testing.T) {
	now := time.Date(2026, 9, 25, 10, 0, 0, 0, msk)
	got, ok := schedule.Next(now, []schedule.Slot{slot(5, 10, 0)}, msk)
	if !ok {
		t.Fatal("want a slot")
	}
	if !got.Equal(now.UTC()) {
		t.Fatalf("the very start of a game must count as future: %v", got.In(msk))
	}
}

func TestNextPicksSoonestOfSeveralSlots(t *testing.T) {
	// Пятница 25.09.2026, 09:00: ср 19:00 (в этом году 30.09) и вс 10:00 (27.09).
	now := time.Date(2026, 9, 25, 9, 0, 0, 0, msk)
	got, ok := schedule.Next(now, []schedule.Slot{slot(3, 19, 0), slot(0, 10, 0)}, msk)
	if !ok {
		t.Fatal("want a slot")
	}
	want := time.Date(2026, 9, 27, 10, 0, 0, 0, msk).UTC()
	if !got.Equal(want) {
		t.Fatalf("got %v want %v", got.In(msk), want.In(msk))
	}
}

func TestNextOrderDoesNotMatter(t *testing.T) {
	now := time.Date(2026, 9, 25, 9, 0, 0, 0, msk)
	a, _ := schedule.Next(now, []schedule.Slot{slot(0, 10, 0), slot(3, 19, 0)}, msk)
	b, _ := schedule.Next(now, []schedule.Slot{slot(3, 19, 0), slot(0, 10, 0)}, msk)
	if !a.Equal(b) {
		t.Fatalf("slot order must not matter: %v vs %v", a, b)
	}
}

func TestNextEmptySchedule(t *testing.T) {
	if _, ok := schedule.Next(time.Now(), nil, msk); ok {
		t.Fatal("an empty schedule has no next game")
	}
}

func TestNextIsUTC(t *testing.T) {
	now := time.Date(2026, 9, 25, 9, 0, 0, 0, msk)
	got, _ := schedule.Next(now, []schedule.Slot{slot(5, 10, 0)}, msk)
	if got.Location() != time.UTC {
		t.Fatalf("games are stored in UTC, got %v", got.Location())
	}
}

func TestParseWeekday(t *testing.T) {
	cases := map[string]int{
		"вс": 0, "воскресенье": 0, "Вс": 0, "вс.": 0,
		"пн": 1, "понедельник": 1,
		"ср": 3, "среду": 3, "СРЕДА": 3,
		"пт": 5, "пятницу": 5,
		"сб": 6, "суббота": 6,
	}
	for word, want := range cases {
		got, ok := schedule.ParseWeekday(word)
		if !ok || got != want {
			t.Fatalf("%q: got %d/%v want %d", word, got, ok, want)
		}
	}
	if _, ok := schedule.ParseWeekday("когда"); ok {
		t.Fatal("not a weekday word")
	}
}

func TestParseTime(t *testing.T) {
	cases := map[string]int{
		"10:00": 600, "10.00": 600, "9:30": 570, "19:45": 1185, "00:00": 0, "23:59": 1439,
	}
	for text, want := range cases {
		got, ok := schedule.ParseTime(text)
		if !ok || got != want {
			t.Fatalf("%q: got %d/%v want %d", text, got, ok, want)
		}
	}
	for _, bad := range []string{"25:00", "10:75", "десять", ""} {
		if _, ok := schedule.ParseTime(bad); ok {
			t.Fatalf("%q must not parse", bad)
		}
	}
}

func TestDescribe(t *testing.T) {
	if got := schedule.Describe(nil); got != "—" {
		t.Fatalf("empty schedule: %q", got)
	}
	slots := []schedule.Slot{slot(3, 19, 0), slot(0, 10, 0)}
	if got := schedule.Describe(slots); got != "ср 19:00, вс 10:00" {
		t.Fatalf("describe: %q", got)
	}
}
