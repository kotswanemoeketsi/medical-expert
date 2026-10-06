// Package job runs scheduled maintenance inside the server process.
package job

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"
)

// Task is one piece of maintenance.
type Task struct {
	Name string
	Run  func(ctx context.Context) (string, error)
}

// Scheduler runs tasks on an interval until its context ends.
type Scheduler struct {
	Log      *slog.Logger
	Interval time.Duration
	Tasks    []Task
	// Quiet logs successful runs at debug level, for a job that runs every
	// minute. Failures are always logged.
	Quiet bool
}

// Start runs every task shortly after start-up and then on each tick.
func (s *Scheduler) Start(ctx context.Context) {
	if s.Interval <= 0 {
		s.Interval = time.Hour
	}
	go func() {
		// A short delay so start-up is not slowed and a crash-looping
		// process does not hammer the database.
		timer := time.NewTimer(30 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				s.runAll(ctx)
				timer.Reset(s.Interval)
			}
		}
	}()
}

func (s *Scheduler) runAll(ctx context.Context) {
	for _, t := range s.Tasks {
		s.runOne(ctx, t)
	}
}

// runOne recovers from panics: a panic in a background goroutine would take
// down a process that is also serving clinicians.
func (s *Scheduler) runOne(ctx context.Context, t Task) {
	defer func() {
		if v := recover(); v != nil {
			s.Log.ErrorContext(ctx, "job panicked", "job", t.Name, "panic", fmt.Sprint(v), "stack", string(debug.Stack()))
		}
	}()
	start := time.Now()
	summary, err := t.Run(ctx)
	if err != nil {
		s.Log.ErrorContext(ctx, "job failed", "job", t.Name, "err", err)
		return
	}
	level := slog.LevelInfo
	if s.Quiet {
		level = slog.LevelDebug
	}
	s.Log.Log(ctx, level, "job finished", "job", t.Name, "result", summary, "ms", time.Since(start).Milliseconds())
}
