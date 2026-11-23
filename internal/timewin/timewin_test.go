package timewin

import (
	"testing"
	"time"

	"kilnalert/internal/model"
)

func mustWindow(t *testing.T, start, end string, days []time.Weekday, zone string, holidays map[string]bool) model.TimeWindow {
	t.Helper()
	w, err := NewWindow(start, end, days, zone, holidays)
	if err != nil {
		t.Fatalf("NewWindow(%s,%s,%s): %v", start, end, zone, err)
	}
	return w
}

func at(t *testing.T, layout, s string, loc *time.Location) time.Time {
	t.Helper()
	x, err := time.ParseInLocation(layout, s, loc)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return x
}

func TestPlainWindow(t *testing.T) {
	w := mustWindow(t, "08:00", "17:00", nil, "Asia/Shanghai", nil)
	loc, _ := time.LoadLocation("Asia/Shanghai")
	cases := []struct {
		at   string
		want bool
	}{
		{"2026-03-02T07:59:59", false},
		{"2026-03-02T08:00:00", true},
		{"2026-03-02T16:59:59", true},
		{"2026-03-02T17:00:00", false},
		{"2026-03-02T23:10:00", false},
	}
	for _, c := range cases {
		got := ActiveAt(w, at(t, "2006-01-02T15:04:05", c.at, loc))
		if got != c.want {
			t.Errorf("%s: got %v want %v", c.at, got, c.want)
		}
	}
}

func TestWrappedWindowAndDays(t *testing.T) {
	// Night shift: 22:00-06:00, Monday-Friday. Monday's shift runs into
	// Tuesday morning and must still be active at 02:00 Tuesday.
	monFri := []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday}
	w := mustWindow(t, "22:00", "06:00", monFri, "Asia/Shanghai", nil)
	loc, _ := time.LoadLocation("Asia/Shanghai")
	cases := []struct {
		at   string
		want bool
	}{
		{"2026-03-02T21:59:59", false}, // Monday evening, before
		{"2026-03-02T22:00:00", true},  // Monday night
		{"2026-03-03T01:30:00", true},  // Tuesday early, Monday's shift
		{"2026-03-03T06:00:00", false}, // off
		{"2026-03-06T23:00:00", true},  // Friday night
		{"2026-03-07T03:00:00", true},  // Saturday early, Friday's shift
		{"2026-03-07T23:00:00", false}, // Saturday night, no shift
		{"2026-03-08T03:00:00", false}, // Sunday early, Saturday had no shift
		{"2026-03-09T05:00:00", true},  // Monday early, Sunday night shift? no: Sunday not eligible -> false
	}
	for _, c := range cases {
		got := ActiveAt(w, at(t, "2006-01-02T15:04:05", c.at, loc))
		want := c.want
		if c.at == "2026-03-09T05:00:00" {
			want = false // Monday 05:00 belongs to Sunday (not a shift day)
		}
		if got != want {
			t.Errorf("%s: got %v want %v", c.at, got, want)
		}
	}
}

func TestTimezoneShift(t *testing.T) {
	// A window defined in UTC midnight-to-noon is 08:00-20:00 in Shanghai.
	w := mustWindow(t, "00:00", "12:00", nil, "UTC", nil)
	shanghai, _ := time.LoadLocation("Asia/Shanghai")
	if !ActiveAt(w, at(t, "2006-01-02T15:04:05", "2026-03-02T09:00:00", shanghai)) {
		t.Error("09:00 Shanghai should be inside 00:00-12:00 UTC")
	}
	w2, err := WithTimezone(w, "America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	la, _ := time.LoadLocation("America/Los_Angeles")
	if !ActiveAt(w2, at(t, "2006-01-02T15:04:05", "2026-03-02T09:00:00", la)) {
		t.Error("09:00 LA should be inside 00:00-12:00 LA after re-zoning")
	}
}

func TestDSTNightShift(t *testing.T) {
	// New York night shift 22:00-06:00 across the spring-forward night
	// (2026-03-08 02:00 -> 03:00). The shift is 7 hours of wall clock that
	// night; 02:30 does not exist but evaluation must still be correct.
	w := mustWindow(t, "22:00", "06:00", nil, "America/New_York", nil)
	ny, _ := time.LoadLocation("America/New_York")
	before := time.Date(2026, 3, 7, 23, 0, 0, 0, ny)
	during := time.Date(2026, 3, 8, 1, 59, 0, 0, ny)
	afterGap := time.Date(2026, 3, 8, 3, 30, 0, 0, ny)
	end := time.Date(2026, 3, 8, 6, 0, 0, 0, ny)
	for _, x := range []time.Time{before, during, afterGap} {
		if !ActiveAt(w, x) {
			t.Errorf("%v should be active", x)
		}
	}
	if ActiveAt(w, end) {
		t.Error("06:00 should be inactive")
	}
}

func TestHolidayWindow(t *testing.T) {
	holidays := map[string]bool{"2026-10-01": true, "2026-10-02": true, "2026-10-03": true}
	w := mustWindow(t, "00:00", "24:00", nil, "Asia/Shanghai", holidays)
	loc, _ := time.LoadLocation("Asia/Shanghai")
	if !ActiveAt(w, at(t, "2006-01-02T15:04", "2026-10-01T13:00", loc)) {
		t.Error("holiday should be active all day")
	}
	if ActiveAt(w, at(t, "2006-01-02T15:04", "2026-10-04T13:00", loc)) {
		t.Error("non-holiday should be inactive")
	}
}

func TestHolidayWrapped(t *testing.T) {
	// Holiday evening 22:00-06:00: the morning after a holiday date is part
	// of that holiday's shift.
	holidays := map[string]bool{"2026-10-01": true}
	w := mustWindow(t, "22:00", "06:00", nil, "Asia/Shanghai", holidays)
	loc, _ := time.LoadLocation("Asia/Shanghai")
	if !ActiveAt(w, at(t, "2006-01-02T15:04", "2026-10-02T03:00", loc)) {
		t.Error("early hours after holiday night should be active")
	}
	if ActiveAt(w, at(t, "2006-01-02T15:04", "2026-10-03T03:00", loc)) {
		t.Error("early hours after a normal night should be inactive")
	}
}

func TestSegmentsExact(t *testing.T) {
	w := mustWindow(t, "22:00", "06:00", nil, "Asia/Shanghai", nil)
	from := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	to := time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)
	segs := Segments([]model.TimeWindow{w}, from, to)
	if len(segs) == 0 {
		t.Fatal("no segments")
	}
	if !segs[0].From.Equal(from) || !segs[len(segs)-1].To.Equal(to) {
		t.Fatal("segments do not cover the range exactly")
	}
	for i := 1; i < len(segs); i++ {
		if !segs[i].From.Equal(segs[i-1].To) {
			t.Fatalf("gap or overlap between segment %d and %d", i-1, i)
		}
		if segs[i].Active == segs[i-1].Active {
			t.Fatalf("adjacent segments %d,%d have the same activity and should be merged", i-1, i)
		}
	}
	// Verify midpoint activity against ActiveAt for every segment.
	for _, s := range segs {
		mid := s.From.Add(s.To.Sub(s.From) / 2)
		if Active([]model.TimeWindow{w}, mid) != s.Active {
			t.Errorf("segment %v..%v claims active=%v", s.From, s.To, s.Active)
		}
	}
	// Boundaries must be exact window edges in local time.
	sh, _ := time.LoadLocation("Asia/Shanghai")
	for _, s := range segs {
		if s.From.Equal(from) {
			continue // the requested range may start mid-window
		}
		local := s.From.In(sh)
		if local.Hour() != 22 && local.Hour() != 6 && local.Hour() != 0 {
			t.Errorf("unexpected boundary %v (local %v)", s.From, local)
		}
	}
}

func TestSegmentsNoWindows(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(48 * time.Hour)
	segs := Segments(nil, from, to)
	if len(segs) != 1 || !segs[0].Active || !segs[0].From.Equal(from) || !segs[0].To.Equal(to) {
		t.Fatalf("unexpected segments: %+v", segs)
	}
}

func TestNextChange(t *testing.T) {
	w := mustWindow(t, "08:00", "17:00", nil, "Asia/Shanghai", nil)
	sh, _ := time.LoadLocation("Asia/Shanghai")
	t0 := time.Date(2026, 3, 2, 9, 0, 0, 0, sh)
	next, ok := NextChange([]model.TimeWindow{w}, t0, t0.Add(48*time.Hour))
	if !ok {
		t.Fatal("no next change")
	}
	want := time.Date(2026, 3, 2, 17, 0, 0, 0, sh)
	if !next.Equal(want) {
		t.Fatalf("next change %v want %v", next, want)
	}
}
