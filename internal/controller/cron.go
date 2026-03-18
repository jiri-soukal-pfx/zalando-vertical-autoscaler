package controller

import (
	"fmt"
	"time"

	"github.com/adhocore/gronx"
)

const (
	// cronRequeueJitter is added to requeue durations to avoid thundering herds.
	cronRequeueJitter = 5 * time.Second
)

// WindowEvaluator evaluates maintenance window cron expressions.
// Supports extended 5-field syntax including L (last), W (weekday), and # (nth).
type WindowEvaluator struct {
	gron *gronx.Gronx
}

// NewWindowEvaluator returns a WindowEvaluator supporting extended 5-field cron
// syntax (L, W, #) in addition to standard expressions.
func NewWindowEvaluator() *WindowEvaluator {
	return &WindowEvaluator{
		gron: gronx.New(),
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
// cronExpr (5-field, with extended syntax support) and timeoutMinutes.
func (e *WindowEvaluator) Evaluate(cronExpr string, timeoutMinutes int, now time.Time) (WindowResult, error) {
	if !e.gron.IsValid(cronExpr) {
		return WindowResult{}, fmt.Errorf("invalid cron expression: %q", cronExpr)
	}

	windowDuration := time.Duration(timeoutMinutes) * time.Minute

	// Find the most recent scheduled fire time at or before now.
	prev, err := gronx.PrevTickBefore(cronExpr, now, true)
	if err != nil {
		// No previous fire found; compute next window.
		next, nextErr := gronx.NextTickAfter(cronExpr, now, false)
		if nextErr != nil {
			return WindowResult{}, fmt.Errorf("finding next fire for %q: %w", cronExpr, nextErr)
		}
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
	next, err := gronx.NextTickAfter(cronExpr, now, false)
	if err != nil {
		return WindowResult{}, fmt.Errorf("finding next fire for %q: %w", cronExpr, err)
	}
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
