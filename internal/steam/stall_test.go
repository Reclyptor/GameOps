package steam

import (
	"errors"
	"testing"
	"time"
)

func TestRunWatchedKillsAStalledProcess(t *testing.T) {
	start := time.Now()
	err := runWatched("bash", []string{"-c", "echo hello; sleep 60"}, 2*time.Second)
	if !errors.Is(err, ErrStalled) {
		t.Fatalf("expected ErrStalled, got %v", err)
	}
	if time.Since(start) > 20*time.Second {
		t.Fatalf("took too long: %s", time.Since(start))
	}
	if err := runWatched("bash", []string{"-c", "for i in 1 2 3; do echo tick; sleep 1; done"}, 3*time.Second); err != nil {
		t.Fatalf("a process that keeps talking must not be killed: %v", err)
	}
	if err := runWatched("bash", []string{"-c", "exit 8"}, 0); err == nil {
		t.Fatal("exit status must propagate")
	}
}
