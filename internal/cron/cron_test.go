package cron

import (
	"testing"
	"time"
)

func TestValidate(t *testing.T) {
	good := []string{
		"* * * * *",
		"*/5 * * * *",
		"0 0 * * *",
		"0 9 * * 1-5",
		"15,45 * * * *",
		"0 0 1 * *",
		"0 0 * * MON",
		"0 0 1 JAN *",
	}
	for _, expr := range good {
		if err := Validate(expr); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", expr, err)
		}
	}

	bad := []string{
		"",            // empty
		"* * * *",     // 4 fields
		"* * * * * *", // 6 fields (seconds not allowed)
		"@daily",      // descriptors disabled
		"60 * * * *",  // minute out of range
		"* 24 * * *",  // hour out of range
		"* * * * 8",   // dow out of range
		"*/0 * * * *", // zero step
		"notacron",    // garbage
	}
	for _, expr := range bad {
		if err := Validate(expr); err == nil {
			t.Errorf("Validate(%q) = nil, want error", expr)
		}
	}
}

func TestNext(t *testing.T) {
	// Reference instant: 2026-07-24 10:32:00 UTC (a Friday).
	base := time.Date(2026, 7, 24, 10, 32, 0, 0, time.UTC)

	cases := []struct {
		name string
		expr string
		want time.Time
	}{
		{
			name: "every minute -> next minute",
			expr: "* * * * *",
			want: time.Date(2026, 7, 24, 10, 33, 0, 0, time.UTC),
		},
		{
			name: "every 5 minutes -> next multiple of 5",
			expr: "*/5 * * * *",
			want: time.Date(2026, 7, 24, 10, 35, 0, 0, time.UTC),
		},
		{
			name: "daily midnight -> next midnight",
			expr: "0 0 * * *",
			want: time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "weekdays 9am -> same day is Friday so 9am already passed, Monday",
			expr: "0 9 * * 1-5",
			want: time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC), // Mon 2026-07-27
		},
		{
			name: "top of next hour",
			expr: "0 * * * *",
			want: time.Date(2026, 7, 24, 11, 0, 0, 0, time.UTC),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Next(tc.expr, base)
			if err != nil {
				t.Fatalf("Next(%q) error: %v", tc.expr, err)
			}
			if !got.Equal(tc.want) {
				t.Errorf("Next(%q, %v) = %v, want %v", tc.expr, base, got, tc.want)
			}
		})
	}
}

// TestNextIsStrictlyAfter proves Next never returns the input instant itself
// (so an advance always moves forward, never re-selecting the just-fired slot).
func TestNextIsStrictlyAfter(t *testing.T) {
	// base sits exactly on a firing boundary for "* * * * *".
	base := time.Date(2026, 7, 24, 10, 0, 0, 0, time.UTC)
	got, err := Next("* * * * *", base)
	if err != nil {
		t.Fatal(err)
	}
	if !got.After(base) {
		t.Errorf("Next returned %v which is not strictly after %v", got, base)
	}
	if want := base.Add(time.Minute); !got.Equal(want) {
		t.Errorf("Next = %v, want %v", got, want)
	}
}

// TestNextNormalizesToUTC proves the cadence is evaluated in UTC even when the
// caller passes a non-UTC time.
func TestNextNormalizesToUTC(t *testing.T) {
	// 2026-07-24 23:30 in a +05:30 zone == 18:00 UTC.
	zone := time.FixedZone("IST", 5*3600+30*60) // +05:30 in seconds
	local := time.Date(2026, 7, 24, 23, 30, 0, 0, zone)
	got, err := Next("0 * * * *", local) // top of next hour, UTC
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 7, 24, 19, 0, 0, 0, time.UTC) // 18:00 UTC -> next top 19:00
	if !got.Equal(want) {
		t.Errorf("Next = %v, want %v (UTC)", got.UTC(), want)
	}
}
