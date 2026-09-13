package state

import (
	"os"
	"os/exec"
	"testing"
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

func TestLock(t *testing.T) {
	s := New(t.TempDir())
	s.Init()
	ran := false
	if err := s.WithLock(func() error { ran = true; return nil }); err != nil || !ran {
		t.Fatal("lock should run fn")
	}
	inner := make(chan struct{})
	release := make(chan struct{})
	go s.WithLock(func() error { close(inner); <-release; return nil })
	<-inner
	if err := s.WithLock(func() error { return nil }); err != ErrLocked {
		t.Fatalf("expected ErrLocked, got %v", err)
	}
	close(release)
}
