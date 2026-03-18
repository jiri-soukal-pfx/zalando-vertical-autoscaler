package controller

import (
	"testing"
	"time"
)

func TestWindowEvaluator_InWindow(t *testing.T) {
	e := NewWindowEvaluator()

	tests := []struct {
		name           string
		cronExpr       string
		timeoutMinutes int
		now            time.Time
		wantInWindow   bool
	}{
		{
			name:           "exactly at window start",
			cronExpr:       "0 2 * * *", // 02:00 every day
			timeoutMinutes: 60,
			now:            time.Date(2024, 1, 15, 2, 0, 0, 0, time.UTC),
			wantInWindow:   true,
		},
		{
			name:           "30 minutes into 60-min window",
			cronExpr:       "0 2 * * *",
			timeoutMinutes: 60,
			now:            time.Date(2024, 1, 15, 2, 30, 0, 0, time.UTC),
			wantInWindow:   true,
		},
		{
			name:           "just before window end",
			cronExpr:       "0 2 * * *",
			timeoutMinutes: 60,
			now:            time.Date(2024, 1, 15, 2, 59, 0, 0, time.UTC),
			wantInWindow:   true,
		},
		{
			name:           "exactly at window end (closed)",
			cronExpr:       "0 2 * * *",
			timeoutMinutes: 60,
			now:            time.Date(2024, 1, 15, 3, 0, 0, 0, time.UTC),
			wantInWindow:   false,
		},
		{
			name:           "one hour before window",
			cronExpr:       "0 2 * * *",
			timeoutMinutes: 60,
			now:            time.Date(2024, 1, 15, 1, 0, 0, 0, time.UTC),
			wantInWindow:   false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := e.Evaluate(tc.cronExpr, tc.timeoutMinutes, tc.now)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result.InWindow != tc.wantInWindow {
				t.Errorf("InWindow=%v, want %v", result.InWindow, tc.wantInWindow)
			}
		})
	}
}

func TestWindowEvaluator_InvalidCron(t *testing.T) {
	e := NewWindowEvaluator()
	_, err := e.Evaluate("invalid cron", 60, time.Now())
	if err == nil {
		t.Fatal("expected error for invalid cron expression")
	}
}

func TestWindowEvaluator_NextOpen(t *testing.T) {
	e := NewWindowEvaluator()
	now := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	// Window opens at 02:00 every day; we're past it, so next is tomorrow.
	result, err := e.Evaluate("0 2 * * *", 60, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.InWindow {
		t.Fatal("expected to be outside window")
	}
	expected := time.Date(2024, 1, 16, 2, 0, 0, 0, time.UTC)
	if !result.NextOpen.Equal(expected) {
		t.Errorf("NextOpen=%v, want %v", result.NextOpen, expected)
	}
}

func TestWindowEvaluator_LastSunday(t *testing.T) {
	e := NewWindowEvaluator()

	// Jan 2024: last Sunday is the 28th
	// Feb 2024: last Sunday is the 25th
	tests := []struct {
		name           string
		cronExpr       string
		timeoutMinutes int
		now            time.Time
		wantInWindow   bool
	}{
		{
			name:           "last Sunday of Jan at window start",
			cronExpr:       "0 20 * * 0L",
			timeoutMinutes: 60,
			now:            time.Date(2024, 1, 28, 20, 0, 0, 0, time.UTC),
			wantInWindow:   true,
		},
		{
			name:           "last Sunday of Jan mid-window",
			cronExpr:       "0 20 * * 0L",
			timeoutMinutes: 60,
			now:            time.Date(2024, 1, 28, 20, 30, 0, 0, time.UTC),
			wantInWindow:   true,
		},
		{
			name:           "non-last Sunday of Jan - outside window",
			cronExpr:       "0 20 * * 0L",
			timeoutMinutes: 60,
			now:            time.Date(2024, 1, 21, 20, 30, 0, 0, time.UTC),
			wantInWindow:   false,
		},
		{
			name:           "last Sunday of Feb 2024",
			cronExpr:       "0 20 * * 0L",
			timeoutMinutes: 60,
			now:            time.Date(2024, 2, 25, 20, 15, 0, 0, time.UTC),
			wantInWindow:   true,
		},
		{
			name:           "day after last Sunday - outside window",
			cronExpr:       "0 20 * * 0L",
			timeoutMinutes: 60,
			now:            time.Date(2024, 1, 29, 10, 0, 0, 0, time.UTC),
			wantInWindow:   false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := e.Evaluate(tc.cronExpr, tc.timeoutMinutes, tc.now)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result.InWindow != tc.wantInWindow {
				t.Errorf("InWindow=%v, want %v", result.InWindow, tc.wantInWindow)
			}
		})
	}
}

func TestWindowEvaluator_LastSunday_NextOpen(t *testing.T) {
	e := NewWindowEvaluator()

	// Jan 29, 2024 (day after last Sunday) -> next should be Feb 25
	now := time.Date(2024, 1, 29, 10, 0, 0, 0, time.UTC)
	result, err := e.Evaluate("0 20 * * 0L", 60, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.InWindow {
		t.Fatal("expected to be outside window")
	}
	expected := time.Date(2024, 2, 25, 20, 0, 0, 0, time.UTC)
	if !result.NextOpen.Equal(expected) {
		t.Errorf("NextOpen=%v, want %v", result.NextOpen, expected)
	}
}

func TestRequeueAfter_InWindow(t *testing.T) {
	now := time.Date(2024, 1, 15, 2, 30, 0, 0, time.UTC)
	windowEnd := time.Date(2024, 1, 15, 3, 0, 0, 0, time.UTC)
	result := WindowResult{
		InWindow:  true,
		WindowEnd: windowEnd,
	}
	d := RequeueAfter(result, now)
	expected := 30*time.Minute + cronRequeueJitter
	if d != expected {
		t.Errorf("RequeueAfter=%v, want %v", d, expected)
	}
}

func TestRequeueAfter_OutsideWindow(t *testing.T) {
	now := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	nextOpen := time.Date(2024, 1, 16, 2, 0, 0, 0, time.UTC)
	result := WindowResult{
		InWindow: false,
		NextOpen: nextOpen,
	}
	d := RequeueAfter(result, now)
	expected := 16*time.Hour + cronRequeueJitter
	if d != expected {
		t.Errorf("RequeueAfter=%v, want %v", d, expected)
	}
}

func TestWindowEvaluator_NthWeekday(t *testing.T) {
e := NewWindowEvaluator()

// "0 20 * * 0#2" = second Sunday of the month at 20:00.
// Jan 2024: 1st Sunday=7th, 2nd Sunday=14th
// Feb 2024: 1st Sunday=4th, 2nd Sunday=11th
tests := []struct {
name         string
now          time.Time
wantInWindow bool
}{
{
name:         "second Sunday of Jan at window start",
now:          time.Date(2024, 1, 14, 20, 0, 0, 0, time.UTC),
wantInWindow: true,
},
{
name:         "second Sunday of Jan mid-window",
now:          time.Date(2024, 1, 14, 20, 30, 0, 0, time.UTC),
wantInWindow: true,
},
{
name:         "first Sunday of Jan - not second Sunday",
now:          time.Date(2024, 1, 7, 20, 15, 0, 0, time.UTC),
wantInWindow: false,
},
{
name:         "third Sunday of Jan - not second Sunday",
now:          time.Date(2024, 1, 21, 20, 15, 0, 0, time.UTC),
wantInWindow: false,
},
{
name:         "second Sunday of Feb 2024",
now:          time.Date(2024, 2, 11, 20, 15, 0, 0, time.UTC),
wantInWindow: true,
},
}

for _, tc := range tests {
t.Run(tc.name, func(t *testing.T) {
result, err := e.Evaluate("0 20 * * 0#2", 60, tc.now)
if err != nil {
t.Fatalf("unexpected error: %v", err)
}
if result.InWindow != tc.wantInWindow {
t.Errorf("InWindow=%v, want %v", result.InWindow, tc.wantInWindow)
}
})
}
}

func TestWindowEvaluator_NearestWeekday(t *testing.T) {
e := NewWindowEvaluator()

// "0 20 15W * *" = nearest weekday to the 15th of each month at 20:00.
// Jan 15 2024 is a Monday (weekday), so fires on Jan 15.
// Jun 15 2024 is a Saturday; nearest weekday is Friday Jun 14.
tests := []struct {
name         string
now          time.Time
wantInWindow bool
}{
{
name:         "Jan 15 2024 is Monday - in window",
now:          time.Date(2024, 1, 15, 20, 0, 0, 0, time.UTC),
wantInWindow: true,
},
{
name:         "Jan 15 2024 mid-window",
now:          time.Date(2024, 1, 15, 20, 45, 0, 0, time.UTC),
wantInWindow: true,
},
{
name:         "Jan 14 2024 Sunday before - outside window",
now:          time.Date(2024, 1, 14, 20, 0, 0, 0, time.UTC),
wantInWindow: false,
},
{
name:         "Jun 14 2024 Friday nearest weekday to Sat Jun 15 - in window",
now:          time.Date(2024, 6, 14, 20, 30, 0, 0, time.UTC),
wantInWindow: true,
},
{
name:         "Jun 15 2024 Saturday itself - outside window",
now:          time.Date(2024, 6, 15, 20, 0, 0, 0, time.UTC),
wantInWindow: false,
},
}

for _, tc := range tests {
t.Run(tc.name, func(t *testing.T) {
result, err := e.Evaluate("0 20 15W * *", 60, tc.now)
if err != nil {
t.Fatalf("unexpected error: %v", err)
}
if result.InWindow != tc.wantInWindow {
t.Errorf("InWindow=%v, want %v", result.InWindow, tc.wantInWindow)
}
})
}
}
