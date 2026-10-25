package cron

import (
	"strings"
	"testing"
	"time"
)

func at(s string) time.Time {
	t, err := time.Parse("2006-01-02 15:04", s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestNext(t *testing.T) {
	tests := []struct{ expr, from, want string }{
		{"* * * * *", "2026-10-25 12:00", "2026-10-25 12:01"},
		{"0 2 * * *", "2026-10-25 12:00", "2026-10-26 02:00"},
		{"0 2 * * *", "2026-10-25 01:59", "2026-10-25 02:00"},
		{"*/15 * * * *", "2026-10-25 12:07", "2026-10-25 12:15"},
		{"5/20 * * * *", "2026-10-25 12:30", "2026-10-25 12:45"},
		{"0 9-17 * * 1-5", "2026-10-24 18:00", "2026-10-26 09:00"}, // Saturday → Monday
		{"30 8 1 * *", "2026-10-25 00:00", "2026-11-01 08:30"},
		{"0 0 29 2 *", "2026-03-01 00:00", "2028-02-29 00:00"},    // leap day
		{"0 0 1,15 * 0", "2026-10-02 00:00", "2026-10-04 00:00"},  // Sunday, or the 1st/15th
		{"0 0 * * 7", "2026-10-25 00:00", "2026-11-01 00:00"},     // 7 is Sunday
		{"59 23 31 12 *", "2026-12-31 23:59", "2027-12-31 23:59"}, // strictly after
		{"0 12 * * *", "2026-10-25 11:59", "2026-10-25 12:00"},
		{"1,2,3 4 * * *", "2026-10-25 04:02", "2026-10-25 04:03"},
	}
	for _, tt := range tests {
		s, err := Parse(tt.expr)
		if err != nil {
			t.Fatalf("%s: %v", tt.expr, err)
		}
		if got := s.Next(at(tt.from)); !got.Equal(at(tt.want)) {
			t.Errorf("%s after %s = %s, want %s", tt.expr, tt.from, got.Format("2006-01-02 15:04 Mon"), tt.want)
		}
	}
}

func TestNextIsUTC(t *testing.T) {
	s, _ := Parse("0 2 * * *")
	berlin := time.FixedZone("CEST", 2*3600)
	got := s.Next(time.Date(2026, 10, 25, 3, 0, 0, 0, berlin)) // 01:00 UTC
	if !got.Equal(at("2026-10-25 02:00")) || got.Location() != time.UTC {
		t.Errorf("got %v", got)
	}
}

func TestParseRejects(t *testing.T) {
	for _, expr := range []string{
		"", "* * * *", "* * * * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * 32 * *", "* * * 13 *",
		"* * * * 8", "5-1 * * * *", "*/0 * * * *", "*/61 * * * *", "a * * * *", "1- * * * *", "-1 * * * *",
		"01 * * * *", "+1 * * * *", "0 0 31 2 *", "0 0 30 2 *", "@daily", "* * * JAN *",
	} {
		if _, err := Parse(expr); err == nil {
			t.Errorf("Parse(%q) accepted", expr)
		}
	}
}

func TestMinGap(t *testing.T) {
	from := at("2026-10-25 00:00")
	for expr, want := range map[string]time.Duration{
		"* * * * *":    time.Minute,
		"*/15 * * * *": 15 * time.Minute,
		"0 2 * * *":    24 * time.Hour,
		"0,30 9 * * *": 30 * time.Minute,
		"0 0 1 * *":    28 * 24 * time.Hour,
		"0 9 * * 1-5":  24 * time.Hour,
		"*/5 9 * * *":  5 * time.Minute,
		"0 */6 * * *":  6 * time.Hour,
		"0 0 1 1 *":    365 * 24 * time.Hour,
	} {
		s, err := Parse(expr)
		if err != nil {
			t.Fatal(err)
		}
		if got := s.MinGap(from); got != want {
			t.Errorf("%s: min gap %s, want %s", expr, got, want)
		}
	}
}

// brute is a minute-by-minute reference implementation.
func brute(s *Schedule, t time.Time) time.Time {
	t = t.UTC().Truncate(time.Minute)
	for i := 0; i < 2*366*24*60; i++ {
		t = t.Add(time.Minute)
		if s.month&(1<<int(t.Month())) != 0 && s.dayMatches(t) && s.hour&(1<<t.Hour()) != 0 && s.minute&(1<<t.Minute()) != 0 {
			return t
		}
	}
	return time.Time{}
}

func FuzzNext(f *testing.F) {
	for _, s := range []string{"* * * * *", "0 2 * * *", "*/7 3-5 1,15 * 2", "0 0 29 2 *", "59 23 * 12 0"} {
		f.Add(s, int64(1792900000))
	}
	f.Fuzz(func(t *testing.T, expr string, unix int64) {
		s, err := Parse(expr)
		if err != nil {
			return
		}
		from := time.Unix(unix%(4e9), 0)
		want := brute(s, from)
		if want.IsZero() {
			return // fires less than every 2 years (leap days): too slow to check
		}
		if got := s.Next(from); !got.Equal(want) {
			t.Fatalf("%q after %v: %v, want %v", expr, from.UTC(), got, want)
		}
		if strings.Count(s.String(), " ") != 4 {
			t.Fatalf("String() = %q", s.String())
		}
	})
}
