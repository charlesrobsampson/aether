package sdk

import (
	"testing"
	"time"
)

func TestToUTC(t *testing.T) {
	// Monday 15 June 2026, 12:00 UTC. In Chicago that is 07:00 CDT and in
	// London it is 13:00 BST. Fixed so "today" and "next Monday" are
	// deterministic.
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		value   string
		zone    string
		want    time.Time
		wantErr bool
	}{
		{
			name:  "RFC3339 with offset",
			value: "2026-06-15T07:30:00-05:00",
			zone:  "America/Chicago",
			want:  time.Date(2026, 6, 15, 12, 30, 0, 0, time.UTC),
		},
		{
			name:  "RFC3339-shaped offset without seconds",
			value: "2026-06-15T07:30-05:00",
			zone:  "America/Chicago",
			want:  time.Date(2026, 6, 15, 12, 30, 0, 0, time.UTC),
		},
		{
			name:  "RFC3339 with Z keeps the instant",
			value: "2026-06-15T07:30:00Z",
			zone:  "Europe/London",
			want:  time.Date(2026, 6, 15, 7, 30, 0, 0, time.UTC),
		},
		{
			name:  "date and time separated by a space",
			value: "2026-06-15 07:30",
			zone:  "America/Chicago",
			want:  time.Date(2026, 6, 15, 12, 30, 0, 0, time.UTC),
		},
		{
			name:  "date and time separated by T",
			value: "2026-06-15T07:30",
			zone:  "America/Chicago",
			want:  time.Date(2026, 6, 15, 12, 30, 0, 0, time.UTC),
		},
		{
			name:  "clock only is today in the user zone",
			value: "08:15",
			zone:  "America/Chicago",
			want:  time.Date(2026, 6, 15, 13, 15, 0, 0, time.UTC),
		},
		{
			name:  "weekday later today",
			value: "Monday 08:15",
			zone:  "America/Chicago",
			want:  time.Date(2026, 6, 15, 13, 15, 0, 0, time.UTC),
		},
		{
			name:  "weekday already passed rolls to next week",
			value: "Monday 06:00",
			zone:  "America/Chicago",
			want:  time.Date(2026, 6, 22, 11, 0, 0, 0, time.UTC),
		},
		{
			name:  "weekday later this week",
			value: "Tuesday 09:00",
			zone:  "America/Chicago",
			want:  time.Date(2026, 6, 16, 14, 0, 0, 0, time.UTC),
		},
		{
			name:  "Chicago before spring forward",
			value: "2026-03-08 01:59",
			zone:  "America/Chicago",
			want:  time.Date(2026, 3, 8, 7, 59, 0, 0, time.UTC),
		},
		{
			name:  "Chicago after spring forward",
			value: "2026-03-08 03:00",
			zone:  "America/Chicago",
			want:  time.Date(2026, 3, 8, 8, 0, 0, 0, time.UTC),
		},
		{
			name:  "London before spring forward",
			value: "2026-03-29 00:59",
			zone:  "Europe/London",
			want:  time.Date(2026, 3, 29, 0, 59, 0, 0, time.UTC),
		},
		{
			name:  "London after spring forward",
			value: "2026-03-29 02:00",
			zone:  "Europe/London",
			want:  time.Date(2026, 3, 29, 1, 0, 0, 0, time.UTC),
		},
		{
			name:    "unknown zone",
			value:   "2026-06-15 07:30",
			zone:    "Mars/Olympus",
			wantErr: true,
		},
		{
			name:    "unrecognised value",
			value:   "tomorrow-ish",
			zone:    "America/Chicago",
			wantErr: true,
		},
		{
			name:    "empty value",
			value:   "   ",
			zone:    "America/Chicago",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ToUTC(tt.value, tt.zone, now)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ToUTC(%q, %q) = %v, want an error", tt.value, tt.zone, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ToUTC(%q, %q): %v", tt.value, tt.zone, err)
			}
			if !got.Equal(tt.want) {
				t.Fatalf("ToUTC(%q, %q) = %v, want %v", tt.value, tt.zone, got, tt.want)
			}
			if got.Location() != time.UTC {
				t.Fatalf("ToUTC(%q, %q) location = %v, want UTC", tt.value, tt.zone, got.Location())
			}
		})
	}
}
