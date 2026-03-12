package controller

import (
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
)

const (
	// cronRequeueJitter is added to requeue durations to avoid thundering herds.
	cronRequeueJitter = 5 * time.Second
)

// WindowEvaluator evaluates maintenance window cron expressions.
type WindowEvaluator struct {
	parser cron.Parser
}

// NewWindowEvaluator returns a WindowEvaluator using 5-field standard cron syntax.
func NewWindowEvaluator() *WindowEvaluator {
	return &WindowEvaluator{
		parser: cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow),
	}
}

// WindowResult holds the result of a window evaluation.
type WindowResult struct {
	// InWindow is true if now falls within an open maintenance window.
	InWindow bool
	// NextOpen is when the next window will open (valid when InWindow is false).
	NextOpen time.Time
	// WindowEnd is when the current (or next) window will close.
	WindowEnd time.Time
}

// Evaluate determines whether now is within the maintenance window defined by
// cronExpr (5-field) and timeoutMinutes.
func (e *WindowEvaluator) Evaluate(cronExpr string, timeoutMinutes int, now time.Time) (WindowResult, error) {
	schedule, err := e.parser.Parse(cronExpr)
	if err != nil {
		return WindowResult{}, fmt.Errorf("parsing cron expression %q: %w", cronExpr, err)
	}

	// Find the most recent scheduled time at or before now.
	// robfig/cron v3 only provides Next(), so we step back one interval to find
	// the previous fire time. We do this by computing Next(now - 2*duration).
	// Since we don't know the interval, we search the last fire within the last
	// 366 days with a step-back of 1 minute.
	prev := previousFire(schedule, now)

	windowDuration := time.Duration(timeoutMinutes) * time.Minute
	if prev.IsZero() {
		// No previous fire found; compute next window.
		next := schedule.Next(now)
		return WindowResult{
			InWindow:  false,
			NextOpen:  next,
			WindowEnd: next.Add(windowDuration),
		}, nil
	}

	windowEnd := prev.Add(windowDuration)
	if now.Before(windowEnd) {
		// We are inside the window.
		return WindowResult{
			InWindow:  true,
			NextOpen:  prev,
			WindowEnd: windowEnd,
		}, nil
	}

	// Outside the window; compute the next one.
	next := schedule.Next(now)
	return WindowResult{
		InWindow:  false,
		NextOpen:  next,
		WindowEnd: next.Add(windowDuration),
	}, nil
}

// RequeueAfter returns the duration to wait before the next reconcile, given
// a WindowResult. When in the window, it returns the remaining window duration.
// When outside, it returns the time until the next window opens plus jitter.
func RequeueAfter(result WindowResult, now time.Time) time.Duration {
	if result.InWindow {
		remaining := result.WindowEnd.Sub(now)
		if remaining < 0 {
			return cronRequeueJitter
		}
		return remaining + cronRequeueJitter
	}
	until := result.NextOpen.Sub(now)
	if until < 0 {
		return cronRequeueJitter
	}
	return until + cronRequeueJitter
}

// previousFire finds the most recent fire time of schedule before or at now.
// It searches backwards in 1-minute steps for up to 1 year.
func previousFire(schedule cron.Schedule, now time.Time) time.Time {
	const maxLookback = 366 * 24 * time.Hour
	// Try each minute step going back.
	// We use Next to find fires in the upcoming period from candidate.
	// candidate starts at now - maxLookback, and we find Next until it crosses now.
	start := now.Add(-maxLookback)
	var last time.Time
	t := start
	for {
		next := schedule.Next(t)
		if next.IsZero() || next.After(now) {
			break
		}
		last = next
		t = next
	}
	return last
}
