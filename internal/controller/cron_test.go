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
