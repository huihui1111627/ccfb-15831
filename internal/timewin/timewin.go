// Package timewin evaluates recurring allow-windows ("this route only on night
// shift", "only on holidays") in an explicit timezone, including windows that
// wrap midnight.
package timewin

import (
	"fmt"
	"sort"
	"time"

	"kilnalert/internal/model"
)

// ParseClock parses "22:00" or "24:00" into a duration since midnight.
func ParseClock(s string) (time.Duration, error) {
	var h, m int
	if n, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil || n != 2 {
		return 0, fmt.Errorf("bad clock %q", s)
	}
	if h < 0 || h > 24 || m < 0 || m > 59 || (h == 24 && m != 0) {
		return 0, fmt.Errorf("clock out of range %q", s)
	}
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute, nil
}

// NewWindow builds a window. days may be nil for "every day"; holidays, when
// non-nil, restricts occurrences to those dates ("2006-01-02" in the window's
// timezone).
func NewWindow(start, end string, days []time.Weekday, locName string, holidays map[string]bool) (model.TimeWindow, error) {
	s, err := ParseClock(start)
	if err != nil {
		return model.TimeWindow{}, err
	}
	e, err := ParseClock(end)
	if err != nil {
		return model.TimeWindow{}, err
	}
	if s == e {
		return model.TimeWindow{}, fmt.Errorf("window %s-%s is empty", start, end)
	}
	loc, err := time.LoadLocation(locName)
	if err != nil {
		return model.TimeWindow{}, fmt.Errorf("timezone %q: %w", locName, err)
	}
	return model.TimeWindow{Start: s, End: e, Days: days, LocationName: locName, Location: loc, Holidays: holidays}, nil
}

// WithTimezone re-zones a window without changing its clock times or days.
func WithTimezone(w model.TimeWindow, locName string) (model.TimeWindow, error) {
	loc, err := time.LoadLocation(locName)
	if err != nil {
		return w, fmt.Errorf("timezone %q: %w", locName, err)
	}
	w.LocationName = locName
	w.Location = loc
	return w, nil
}

// occurrenceStart returns the local date-time at which the window occurrence
// containing t opened, plus eligibility of that occurrence's date.
func occurrenceStart(w model.TimeWindow, local time.Time) (time.Time, bool) {
	if w.Start == 0 && w.End == 24*time.Hour {
		return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, local.Location()), eligible(w, local)
	}
	tod := clock(local)
	if w.Start < w.End {
		return dateAt(local, w.Start), eligible(w, local)
	}
	// Wraps midnight. Before End belongs to yesterday's occurrence.
	if tod < w.End {
		y := local.AddDate(0, 0, -1)
		return dateAt(y, w.Start), eligible(w, y)
	}
	return dateAt(local, w.Start), eligible(w, local)
}

// eligible reports whether an occurrence opened on this local date may fire.
func eligible(w model.TimeWindow, day time.Time) bool {
	if len(w.Days) > 0 {
		ok := false
		for _, d := range w.Days {
			if d == day.Weekday() {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if w.Holidays != nil && !w.Holidays[day.Format("2006-01-02")] {
		return false
	}
	return true
}

func dateAt(local time.Time, tod time.Duration) time.Time {
	y, m, d := local.Date()
	h := int(tod / time.Hour)
	mi := int((tod % time.Hour) / time.Minute)
	return time.Date(y, m, d, h, mi, 0, 0, local.Location())
}

func clock(t time.Time) time.Duration {
	return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute +
		time.Duration(t.Second())*time.Second + time.Duration(t.Nanosecond())
}

// ActiveAt reports whether the window is active at instant t.
func ActiveAt(w model.TimeWindow, t time.Time) bool {
	if w.Location == nil {
		return true
	}
	local := t.In(w.Location)
	if !eligibleDate(w, local) {
		return false
	}
	return clockOnly(w, local)
}

// eligibleDate checks date eligibility, looking at the previous date for the
// after-midnight half of a wrapped window.
func eligibleDate(w model.TimeWindow, local time.Time) bool {
	if w.Start < w.End || clock(local) >= w.Start {
		return eligible(w, local)
	}
	return eligible(w, local.AddDate(0, 0, -1))
}

func clockOnly(w model.TimeWindow, local time.Time) bool {
	if w.Start == 0 && w.End == 24*time.Hour {
		return true
	}
	tod := clock(local)
	if w.Start < w.End {
		return tod >= w.Start && tod < w.End
	}
	return tod >= w.Start || tod < w.End
}

// Active reports whether any window in the set is active at t. No windows
// means no restriction, i.e. always active.
func Active(windows []model.TimeWindow, t time.Time) bool {
	if len(windows) == 0 {
		return true
	}
	for _, w := range windows {
		if ActiveAt(w, t) {
			return true
		}
	}
	return false
}

// Segment is a maximal interval on which activity is constant.
type Segment struct {
	From, To time.Time
	Active   bool
}

// Segments splits [from,to) into constant-activity segments. Candidate
// boundaries are each window's occurrence start/end and local midnight, in
// the window's own timezone; activity is sampled strictly between
// consecutive boundaries. This makes wrapped windows, multiple timezones and
// DST transitions exact without materializing a calendar of offsets.
func Segments(windows []model.TimeWindow, from, to time.Time) []Segment {
	if !to.After(from) {
		return nil
	}
	if len(windows) == 0 {
		return []Segment{{From: from, To: to, Active: true}}
	}
	points := map[int64]time.Time{from.UnixNano(): from, to.UnixNano(): to}
	add := func(p time.Time) {
		if p.After(from) && p.Before(to) {
			points[p.UnixNano()] = p
		}
	}
	for _, w := range windows {
		loc := w.Location
		if loc == nil {
			continue
		}
		y, m, d := from.In(loc).Date()
		day := time.Date(y, m, d, 0, 0, 0, 0, loc).AddDate(0, 0, -1)
		for !day.After(to) {
			add(day) // local midnight: day-based eligibility may change
			if w.Start == 0 && w.End == 24*time.Hour {
				add(day.AddDate(0, 0, 1))
			} else {
				add(dateAt(day, w.Start))
				if w.Start < w.End {
					add(dateAt(day, w.End))
				} else {
					add(dateAt(day, w.Start))
					add(dateAt(day, w.End))
				}
			}
			day = day.AddDate(0, 0, 1)
		}
	}
	list := make([]time.Time, 0, len(points))
	for _, p := range points {
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Before(list[j]) })
	out := make([]Segment, 0, len(list)-1)
	for i := 0; i+1 < len(list); i++ {
		a, b := list[i], list[i+1]
		if !b.After(a) {
			continue
		}
		mid := a.Add(b.Sub(a) / 2)
		active := Active(windows, mid)
		if n := len(out); n > 0 && out[n-1].Active == active && out[n-1].To.Equal(a) {
			out[n-1].To = b
			continue
		}
		out = append(out, Segment{From: a, To: b, Active: active})
	}
	return out
}

// FirstActiveAfter returns the first instant >= from at which the set is
// active, within horizon.
func FirstActiveAfter(windows []model.TimeWindow, from, horizon time.Time) (time.Time, bool) {
	for _, s := range Segments(windows, from, horizon) {
		if s.Active {
			return s.From, true
		}
	}
	return time.Time{}, false
}

// NextChange returns the first instant after t at which activity changes
// (within horizon), or false if none.
func NextChange(windows []model.TimeWindow, t, horizon time.Time) (time.Time, bool) {
	for _, s := range Segments(windows, t, horizon) {
		if s.From.After(t) {
			return s.From, true
		}
		if !s.To.After(t) {
			continue
		}
		return s.To, s.To.Before(horizon)
	}
	return time.Time{}, false
}

// Describe renders a window for display.
func Describe(w model.TimeWindow) string {
	days := "每天"
	names := []string{"日", "一", "二", "三", "四", "五", "六"}
	if len(w.Days) > 0 {
		parts := make([]string, 0, len(w.Days))
		for _, d := range w.Days {
			parts = append(parts, names[int(d)])
		}
		days = "周" + join(parts, "、")
	}
	zone := w.LocationName
	if zone == "" {
		zone = "UTC"
	}
	s := fmt.Sprintf("%s–%s %s (%s)", fmtClock(w.Start), fmtClock(w.End), days, zone)
	if w.Holidays != nil {
		s += "，仅节假日"
	}
	return s
}

func join(xs []string, sep string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += sep
		}
		out += x
	}
	return out
}

func fmtClock(d time.Duration) string {
	return fmt.Sprintf("%02d:%02d", int(d/time.Hour), int((d%time.Hour)/time.Minute))
}
