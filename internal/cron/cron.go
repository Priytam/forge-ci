// Package cron parses standard 5-field cron expressions and computes next-run
// times for Forge's scheduled pipelines. It is a thin, well-scoped wrapper over
// github.com/robfig/cron/v3 (the standard, well-vetted Go cron parser) so the
// rest of the codebase depends on one small surface.
//
// Expressions are the standard 5 fields — minute hour day-of-month month
// day-of-week — with the usual ranges (1-5), lists (1,3,5), steps (*/2) and
// month/day names (jan, mon). Seconds are NOT part of the expression.
//
// All next-run computation is done in UTC: callers pass UTC times and get UTC
// times back, so a schedule's cadence is timezone-independent and reproducible
// across replicas. This is the documented timezone for scheduled pipelines.
package cron

import (
	"fmt"
	"time"

	robfig "github.com/robfig/cron/v3"
)

// parser accepts the standard 5-field form (minute hour dom month dow) with
// descriptors disabled — an explicit expression is always required so a typo
// like "@dailyy" is rejected rather than silently accepted.
var parser = robfig.NewParser(
	robfig.Minute | robfig.Hour | robfig.Dom | robfig.Month | robfig.Dow,
)

// Parse validates a 5-field cron expression, returning the compiled schedule or
// an error describing why it is invalid.
func Parse(expr string) (robfig.Schedule, error) {
	sched, err := parser.Parse(expr)
	if err != nil {
		return nil, fmt.Errorf("invalid cron expression %q: %w", expr, err)
	}
	return sched, nil
}

// Validate reports whether expr is a valid 5-field cron expression.
func Validate(expr string) error {
	_, err := Parse(expr)
	return err
}

// Next returns the next activation time strictly after `after`, computed in UTC.
// `after` is normalized to UTC so the cadence is always evaluated in UTC
// regardless of the caller's clock location.
func Next(expr string, after time.Time) (time.Time, error) {
	sched, err := Parse(expr)
	if err != nil {
		return time.Time{}, err
	}
	return sched.Next(after.UTC()), nil
}
