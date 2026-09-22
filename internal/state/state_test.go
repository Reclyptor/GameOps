package state

import (
	"errors"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"
)

func TestPlayersAndFlags(t *testing.T) {
	s := New(t.TempDir())
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	s.PlayersAdd("Alice")
	s.PlayersAdd("Alice")
	s.PlayersAdd("Bob Smith")
	if s.PlayersCount() != 2 {
		t.Fatalf("count %d", s.PlayersCount())
	}
	s.PlayersRemove("Alice")
	if p := s.Players(); len(p) != 1 || p[0] != "Bob Smith" {
		t.Fatalf("players %v", p)
	}
	s.PlayersRemove("Nobody")
	s.PlayersClear()
	if s.PlayersCount() != 0 {
		t.Fatal("expected empty")
	}

	s.SetFlag("update.requested", "1.1.0")
	if !s.HasFlag("update.requested") || s.Flag("update.requested") != "1.1.0" {
		t.Fatal("flag round-trip failed")
	}
	s.ClearFlag("update.requested")
	if s.HasFlag("update.requested") {
		t.Fatal("flag should be gone")
	}
	s.SetFlag("stop.requested", "")
	s.PlayersAdd("Ghost")
	s.Init()
	if s.HasFlag("stop.requested") || s.PlayersCount() != 0 {
		t.Fatal("Init must clear leftovers")
	}
}

func TestServerPID(t *testing.T) {
	s := New(t.TempDir())
	s.Init()
	if s.ServerPID() != 0 {
		t.Fatal("expected no pid")
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	s.SetServerPID(cmd.Process.Pid)
	if s.ServerPID() != cmd.Process.Pid {
		t.Fatal("expected live pid")
	}
	cmd.Process.Kill()
	cmd.Wait()
	if s.ServerPID() != 0 {
		t.Fatal("expected dead pid to read as 0")
	}
	os.Remove(s.path("server.pid"))
}

func fastLockPoll(t *testing.T) {
	old := lockPoll
	lockPoll = 5 * time.Millisecond
	t.Cleanup(func() { lockPoll = old })
}

func TestAcquireFree(t *testing.T) {
	s := New(t.TempDir())
	s.Init()
	h, err := s.Acquire(LockOpts{})
	if err != nil {
		t.Fatalf("a free lock must be taken at once: %v", err)
	}
	h.Release()
	h.Release() // idempotent
	var nothing *Held
	nothing.Release() // safe on nil
	h2, err := s.Acquire(LockOpts{})
	if err != nil {
		t.Fatalf("released lock must be free again: %v", err)
	}
	h2.Release()
}

// The regression this exists for: a job arriving while another holds the
// lock used to be skipped. It must wait, then run.
func TestAcquireWaitsForHolder(t *testing.T) {
	fastLockPoll(t)
	s := New(t.TempDir())
	s.Init()
	holder, err := s.Acquire(LockOpts{})
	if err != nil {
		t.Fatal(err)
	}
	var waited atomic.Int32
	got := make(chan error, 1)
	go func() {
		h, err := s.Acquire(LockOpts{Timeout: 5 * time.Second, Waiting: func() { waited.Add(1) }})
		h.Release()
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("must not get the lock while it is held (err=%v)", err)
	case <-time.After(50 * time.Millisecond):
	}
	holder.Release()
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("waiter must get the lock once it is released: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter never got the lock")
	}
	if n := waited.Load(); n != 1 {
		t.Fatalf("Waiting must be called exactly once, got %d", n)
	}
}

func TestAcquireTimesOut(t *testing.T) {
	fastLockPoll(t)
	s := New(t.TempDir())
	s.Init()
	holder, _ := s.Acquire(LockOpts{})
	defer holder.Release()

	start := time.Now()
	h, err := s.Acquire(LockOpts{Timeout: 60 * time.Millisecond})
	if !errors.Is(err, ErrLockTimeout) || h != nil {
		t.Fatalf("expected ErrLockTimeout, got %v", err)
	}
	if time.Since(start) < 60*time.Millisecond {
		t.Fatal("gave up before the timeout")
	}

	// Timeout 0 means a single attempt.
	if _, err := s.Acquire(LockOpts{}); !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("zero timeout on a held lock must fail at once, got %v", err)
	}
}

func TestAcquireGivesUpWhenStopping(t *testing.T) {
	fastLockPoll(t)
	s := New(t.TempDir())
	s.Init()

	// Stopping before the attempt: nothing starts, even with the lock free.
	s.SetFlag("stop.requested", "")
	if !s.Stopping() {
		t.Fatal("Stopping should report the flag")
	}
	if _, err := s.Acquire(LockOpts{Timeout: time.Second, Abort: s.Stopping}); !errors.Is(err, ErrStopping) {
		t.Fatalf("no job may start once stopping, got %v", err)
	}
	s.ClearFlag("stop.requested")

	// Stopping while waiting: the waiter gives up promptly.
	holder, _ := s.Acquire(LockOpts{})
	defer holder.Release()
	got := make(chan error, 1)
	go func() {
		_, err := s.Acquire(LockOpts{Timeout: 10 * time.Second, Abort: s.Stopping})
		got <- err
	}()
	time.Sleep(30 * time.Millisecond)
	s.SetFlag("stop.requested", "")
	select {
	case err := <-got:
		if !errors.Is(err, ErrStopping) {
			t.Fatalf("expected ErrStopping, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter did not notice the stop")
	}
}

func TestQuiescingCoversStoppingAndDraining(t *testing.T) {
	s := New(t.TempDir())
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	if s.Quiescing() {
		t.Fatal("a fresh store must let jobs start")
	}

	// A drain blocks new jobs without the container going down.
	if err := s.SetDraining(); err != nil {
		t.Fatal(err)
	}
	if !s.Draining() || !s.Quiescing() || s.Stopping() {
		t.Fatalf("draining: Draining=%v Quiescing=%v Stopping=%v", s.Draining(), s.Quiescing(), s.Stopping())
	}

	// A cancelled eviction must leave the scheduler working.
	s.ClearDraining()
	if s.Draining() || s.Quiescing() {
		t.Fatal("clearing the drain must let jobs start again")
	}

	s.SetFlag("stop.requested", "")
	if !s.Quiescing() {
		t.Fatal("stopping must block new jobs")
	}

	// Init is what runs on a fresh container: a drain flag left behind by a
	// SIGKILLed drain must not silence the next container's scheduler.
	if err := s.SetDraining(); err != nil {
		t.Fatal(err)
	}
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	if s.Draining() || s.Quiescing() {
		t.Fatal("Init must clear a stale drain flag")
	}
}
