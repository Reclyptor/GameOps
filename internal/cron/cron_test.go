package cron

import (
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

func TestParseAndNext(t *testing.T) {
	cases := []struct{ expr, from, want string }{
		{"0 4 * * *", "2026-09-13 03:10", "2026-09-13 04:00"},
		{"0 4 * * *", "2026-09-13 04:00", "2026-09-14 04:00"},
		{"0 * * * *", "2026-09-13 03:10", "2026-09-13 04:00"},
		{"*/15 * * * *", "2026-09-13 03:10", "2026-09-13 03:15"},
		{"30 2 * * 1", "2026-09-13 03:10", "2026-09-14 02:30"}, // Sunday → Monday
		{"0 0 1 * *", "2026-09-13 03:10", "2026-10-01 00:00"},
		{"0 12 15 * 3", "2026-09-13 03:10", "2026-09-15 12:00"}, // dom OR dow
		{"5,35 8-9 * * *", "2026-09-13 08:06", "2026-09-13 08:35"},
		{"0 0 * 2 *", "2026-09-13 00:00", "2027-02-01 00:00"},
		{"0 22 * * 7", "2026-09-12 23:00", "2026-09-13 22:00"}, // 7 = Sunday
	}
	for _, c := range cases {
		s, err := Parse(c.expr)
		if err != nil {
			t.Fatalf("%s: %v", c.expr, err)
		}
		if got := s.Next(at(c.from)); got != at(c.want) {
			t.Errorf("%s from %s: got %s want %s", c.expr, c.from, got.Format("2006-01-02 15:04"), c.want)
		}
	}
}

func TestParseRejects(t *testing.T) {
	for _, expr := range []string{"", "0 4 * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * * 13 *", "a * * * *", "*/0 * * * *", "5-3 * * * *"} {
		if _, err := Parse(expr); err == nil {
			t.Errorf("expected error for %q", expr)
		}
	}
}
