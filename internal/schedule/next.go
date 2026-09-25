package schedule

import "time"

// Next returns the nearest future game start among the slots, as a UTC
// instant: games are stored in UTC, while slots are written in the chat's
// timezone.
//
// A slot whose weekday is today and whose time has not passed yet wins;
// otherwise the slot repeats in a week. ok is false when the schedule is
// empty, and then the caller falls back to its own default.
func Next(now time.Time, slots []Slot, loc *time.Location) (time.Time, bool) {
	if len(slots) == 0 {
		return time.Time{}, false
	}

	local := now.In(loc)
	midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)

	var best time.Time
	for _, slot := range slots {
		daysAhead := (slot.Weekday - int(local.Weekday()) + 7) % 7
		candidate := midnight.AddDate(0, 0, daysAhead).
			Add(time.Duration(slot.Minutes) * time.Minute)
		if candidate.Before(local) {
			candidate = candidate.AddDate(0, 0, 7)
		}
		if best.IsZero() || candidate.Before(best) {
			best = candidate
		}
	}
	return best.UTC(), true
}
