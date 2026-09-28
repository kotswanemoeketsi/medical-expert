package job

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestRunOneSurvivesPanicsAndErrors(t *testing.T) {
	var buf bytes.Buffer
	s := &Scheduler{Log: slog.New(slog.NewTextHandler(&buf, nil))}
	ran := false
	s.runAll(context.Background()) // no tasks: nothing happens
	s.Tasks = []Task{
		{Name: "boom", Run: func(context.Context) (string, error) { panic("kaboom") }},
		{Name: "fails", Run: func(context.Context) (string, error) { return "", errors.New("nope") }},
		{Name: "works", Run: func(context.Context) (string, error) { ran = true; return "done", nil }},
	}
	s.runAll(context.Background())
	if !ran {
		t.Fatal("a panicking task stopped the tasks after it")
	}
	out := buf.String()
	for _, want := range []string{"job panicked", "kaboom", "job failed", "job finished"} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q:\n%s", want, out)
		}
	}
}
