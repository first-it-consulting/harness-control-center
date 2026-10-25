// Package cron parses standard 5-field cron expressions (minute, hour,
// day of month, month, day of week) and computes when they fire, in UTC.
//
// Supported per field: "*", numbers, ranges "a-b", steps "*/n" and
// "a-b/n", and comma-separated lists of those. Day of week is 0-7 (0 and
// 7 are Sunday). As in Vixie cron, if both day of month and day of week
// are restricted, a day matches if either does. Names (JAN, MON) and
// macros (@daily) are not supported.
package cron

import (
	"errors"
	"fmt"
	"math/bits"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed expression.
type Schedule struct {
	expr             string
	minute, hour     uint64 // bit i set: value i matches
	dom, month, dow  uint64
	domStar, dowStar bool
}

type field struct {
	name     string
	min, max int
}

var fields = [5]field{
	{"minute", 0, 59}, {"hour", 0, 23}, {"day of month", 1, 31}, {"month", 1, 12}, {"day of week", 0, 7},
}

// Parse parses a 5-field expression.
func Parse(expr string) (*Schedule, error) {
	parts := strings.Fields(expr)
	if len(parts) != 5 {
		return nil, fmt.Errorf("cron: %q must have 5 fields (minute hour day-of-month month day-of-week)", expr)
	}
	s := &Schedule{expr: strings.Join(parts, " ")}
	targets := [5]*uint64{&s.minute, &s.hour, &s.dom, &s.month, &s.dow}
	var errs []error
	for i, p := range parts {
		set, err := parseField(p, fields[i])
		if err != nil {
			errs = append(errs, err)
			continue
		}
		*targets[i] = set
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	if s.dow&(1<<7) != 0 { // 7 is Sunday too
		s.dow = s.dow&^(1<<7) | 1
	}
	s.domStar = parts[2] == "*" || strings.HasPrefix(parts[2], "*/")
	s.dowStar = parts[4] == "*" || strings.HasPrefix(parts[4], "*/")
	if !s.canFire() {
		return nil, fmt.Errorf("cron: %q never fires", expr)
	}
	return s, nil
}

func parseField(p string, f field) (uint64, error) {
	var set uint64
	for _, item := range strings.Split(p, ",") {
		rng, stepStr, hasStep := strings.Cut(item, "/")
		lo, hi := f.min, f.max
		switch {
		case rng == "*":
		case strings.Contains(rng, "-"):
			a, b, _ := strings.Cut(rng, "-")
			var err1, err2 error
			lo, err1 = number(a, f)
			hi, err2 = number(b, f)
			if err := errors.Join(err1, err2); err != nil {
				return 0, err
			}
			if lo > hi {
				return 0, fmt.Errorf("cron: %s range %q is backwards", f.name, rng)
			}
		default:
			n, err := number(rng, f)
			if err != nil {
				return 0, err
			}
			lo, hi = n, n
			if hasStep { // "5/15" means 5-max/15
				hi = f.max
			}
		}
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepStr)
			if err != nil || n < 1 || n > f.max {
				return 0, fmt.Errorf("cron: %s step %q must be between 1 and %d", f.name, stepStr, f.max)
			}
			step = n
		}
		for v := lo; v <= hi; v += step {
			set |= 1 << v
		}
	}
	return set, nil
}

func number(s string, f field) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < f.min || n > f.max || s != strconv.Itoa(n) {
		return 0, fmt.Errorf("cron: %s %q must be a number between %d and %d", f.name, s, f.min, f.max)
	}
	return n, nil
}

// canFire reports whether some day of some month matches (e.g. "0 0 31
// 2 *" never does).
func (s *Schedule) canFire() bool {
	// Searching 8 years covers every weekday/leap-year combination.
	_, ok := s.next(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), 8)
	return ok
}

// String returns the normalized expression.
func (s *Schedule) String() string { return s.expr }

// Next returns the first firing time strictly after t, in UTC.
func (s *Schedule) Next(t time.Time) time.Time {
	n, ok := s.next(t, 8)
	if !ok {
		return time.Time{} // unreachable: Parse rejects schedules that never fire
	}
	return n
}

func (s *Schedule) next(t time.Time, years int) (time.Time, bool) {
	t = t.UTC().Truncate(time.Minute).Add(time.Minute)
	end := t.AddDate(years, 0, 0)
	for t.Before(end) {
		if s.month&(1<<int(t.Month())) == 0 {
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, time.UTC)
			continue
		}
		if !s.dayMatches(t) {
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, time.UTC)
			continue
		}
		if s.hour&(1<<t.Hour()) == 0 {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, time.UTC)
			continue
		}
		if s.minute&(1<<t.Minute()) == 0 {
			// Jump to the next matching minute in this hour, if any.
			rest := s.minute >> (t.Minute() + 1)
			if rest == 0 {
				t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, time.UTC)
				continue
			}
			t = t.Add(time.Duration(bits.TrailingZeros64(rest)+1) * time.Minute)
			continue
		}
		return t, true
	}
	return time.Time{}, false
}

func (s *Schedule) dayMatches(t time.Time) bool {
	dom := s.dom&(1<<t.Day()) != 0
	dow := s.dow&(1<<int(t.Weekday())) != 0
	switch {
	case s.domStar && s.dowStar:
		return true
	case s.domStar:
		return dow
	case s.dowStar:
		return dom
	default:
		return dom || dow
	}
}

// MinGap is the shortest time between two consecutive firings within
// the next year after from (a schedule's effective frequency).
func (s *Schedule) MinGap(from time.Time) time.Duration {
	prev := s.Next(from)
	end := prev.AddDate(1, 0, 0)
	gap := time.Duration(1<<63 - 1)
	for i := 0; i < 600_000 && prev.Before(end); i++ {
		n := s.Next(prev)
		if d := n.Sub(prev); d < gap {
			gap = d
		}
		if gap == time.Minute {
			break
		}
		prev = n
	}
	return gap
}
