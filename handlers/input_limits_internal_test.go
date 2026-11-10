package handlers

import (
	"testing"
	"time"
)

func TestParseBirthDate(t *testing.T) {
	today := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		in, code string
	}{
		{"2008-11-10", ""},         // 18 today
		{"2008-11-11", "underage"}, // 18 tomorrow
		{"1990-02-28", ""},
		{"2030-01-01", "birth_date_invalid"},
		{"1900-01-01", "birth_date_invalid"},
		{"10/11/2000", "birth_date_invalid"},
		{"", "birth_date_invalid"},
	}
	for _, c := range cases {
		_, code, _ := parseBirthDate(c.in, today)
		if code != c.code {
			t.Errorf("parseBirthDate(%q) code = %q, want %q", c.in, code, c.code)
		}
	}
	// 29 Feb birthday: 18 on 1 Mar in a non-leap year.
	b := time.Date(2008, 2, 29, 0, 0, 0, 0, time.UTC)
	if got := ageOn(b, time.Date(2026, 2, 28, 0, 0, 0, 0, time.UTC)); got != 17 {
		t.Errorf("age on 28 Feb = %d, want 17", got)
	}
	if got := ageOn(b, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)); got != 18 {
		t.Errorf("age on 1 Mar = %d, want 18", got)
	}
}
