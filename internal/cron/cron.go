// Package cron parses five-field cron expressions and computes the next
// matching time. It supports *, lists, ranges and steps in every field,
// which is what the environment contract promises and nothing more.
package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type field struct {
	set [60]bool
	min int
	max int
}

// Schedule is a parsed expression: minute hour day-of-month month day-of-week.
type Schedule struct {
	minute, hour, dom, month, dow field
	domStar, dowStar              bool
	src                           string
}

func (s *Schedule) String() string { return s.src }

// Parse accepts "m h dom mon dow"; each field is *, n, a-b, a,b,c, */n or a-b/n.
func Parse(expr string) (*Schedule, error) {
	parts := strings.Fields(expr)
	if len(parts) != 5 {
		return nil, fmt.Errorf("expected 5 fields, got %d in %q", len(parts), expr)
	}
	s := &Schedule{src: strings.Join(parts, " ")}
	var err error
	if s.minute, err = parseField(parts[0], 0, 59); err != nil {
		return nil, fmt.Errorf("minute: %w", err)
	}
	if s.hour, err = parseField(parts[1], 0, 23); err != nil {
		return nil, fmt.Errorf("hour: %w", err)
	}
	if s.dom, err = parseField(parts[2], 1, 31); err != nil {
		return nil, fmt.Errorf("day of month: %w", err)
	}
	if s.month, err = parseField(parts[3], 1, 12); err != nil {
		return nil, fmt.Errorf("month: %w", err)
	}
	if s.dow, err = parseField(parts[4], 0, 7); err != nil {
		return nil, fmt.Errorf("day of week: %w", err)
	}
	// 7 is Sunday too.
	if s.dow.set[7] {
		s.dow.set[0] = true
	}
	s.domStar = parts[2] == "*"
	s.dowStar = parts[4] == "*"
	return s, nil
}

func parseField(spec string, lo, hi int) (field, error) {
	f := field{min: lo, max: hi}
	for _, part := range strings.Split(spec, ",") {
		step := 1
		if i := strings.IndexByte(part, '/'); i >= 0 {
			n, err := strconv.Atoi(part[i+1:])
			if err != nil || n <= 0 {
				return f, fmt.Errorf("bad step in %q", part)
			}
			step = n
			part = part[:i]
		}
		a, b := lo, hi
		switch {
		case part == "*":
		case strings.Contains(part, "-"):
			r := strings.SplitN(part, "-", 2)
			var err error
			if a, err = strconv.Atoi(r[0]); err != nil {
				return f, fmt.Errorf("bad range %q", part)
			}
			if b, err = strconv.Atoi(r[1]); err != nil {
				return f, fmt.Errorf("bad range %q", part)
			}
		default:
			n, err := strconv.Atoi(part)
			if err != nil {
				return f, fmt.Errorf("bad value %q", part)
			}
			a, b = n, n
			if step > 1 {
				b = hi
			}
		}
		if a < lo || b > hi || a > b {
			return f, fmt.Errorf("%q out of range %d-%d", part, lo, hi)
		}
		for v := a; v <= b; v += step {
			f.set[v] = true
		}
	}
	return f, nil
}

// Matches reports whether t (truncated to the minute) satisfies the schedule.
// Day-of-month and day-of-week combine the way cron does: when both are
// restricted, either may match.
func (s *Schedule) Matches(t time.Time) bool {
	if !s.minute.set[t.Minute()] || !s.hour.set[t.Hour()] || !s.month.set[int(t.Month())] {
		return false
	}
	dom := s.dom.set[t.Day()]
	dow := s.dow.set[int(t.Weekday())]
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

// Next returns the first matching minute strictly after t.
func (s *Schedule) Next(t time.Time) time.Time {
	t = t.Truncate(time.Minute).Add(time.Minute)
	// Five years is far beyond any real schedule; it bounds pathological input.
	limit := t.AddDate(5, 0, 0)
	for t.Before(limit) {
		if s.Matches(t) {
			return t
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}
}
