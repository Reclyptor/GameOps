// Package state is the ephemeral runtime state under GAMEOPS_STATE
// (CONTRACT.md §8): the server pid, request flags, the shared lock and the
// tracked player set. Files, not memory, because cron jobs and the health
// check are separate processes that must see the runner's state.
package state

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Store struct{ Dir string }

func New(dir string) *Store { return &Store{Dir: dir} }

func (s *Store) path(name string) string { return filepath.Join(s.Dir, name) }

// Init is called once, by the runner, at boot. Anything left over is from a
// previous life of this container and must not influence this one.
func (s *Store) Init() error {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return fmt.Errorf("cannot create state dir %s: %w", s.Dir, err)
	}
	for _, f := range []string{"server.pid", "server.rc", "server.started", "server.version", "ready", "players",
		"update.requested", "update.available", "update.failed", "restore.requested", "stop.requested", "console.fifo"} {
		os.Remove(s.path(f))
	}
	os.RemoveAll(s.path("counters"))
	if err := os.MkdirAll(s.path("counters"), 0o755); err != nil {
		return err
	}
	return os.WriteFile(s.path("players"), nil, 0o644)
}

// SetServerStarted stamps the launch time of the current server process.
func (s *Store) SetServerStarted() error {
	return os.WriteFile(s.path("server.started"), []byte(strconv.FormatInt(time.Now().Unix(), 10)), 0o644)
}

// ServerStarted returns the launch time (unix seconds), or 0 when unknown.
func (s *Store) ServerStarted() int64 {
	n, _ := strconv.ParseInt(s.Flag("server.started"), 10, 64)
	return n
}

// The counters /metrics exposes. Files under counters/, one integer each,
// guarded by a lock so the runner, cron jobs and the health check can all
// bump them; they reset with the container.
const (
	BackupsTotal        = "backups_total"
	BackupFailuresTotal = "backup_failures_total"
	UpdatesTotal        = "updates_total"
	RestoresTotal       = "restores_total"
	NotifyFailuresTotal = "notify_failures_total"
	ServerRestartsTotal = "server_restarts_total"
	GatePassedTotal     = "gate_passed_total"
	GateDroppedTotal    = "gate_dropped_total"
)

func (s *Store) counterPath(name string) string { return filepath.Join(s.path("counters"), name) }

func (s *Store) withCounterLock(fn func() error) error {
	if err := os.MkdirAll(s.path("counters"), 0o755); err != nil {
		return err
	}
	lf, err := os.OpenFile(s.path("counters.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
	return fn()
}

// Bump increments a counter. Never fatal: metrics must not break the job
// that is bumping them.
func (s *Store) Bump(name string) {
	_ = s.withCounterLock(func() error {
		n := s.Counter(name)
		tmp := s.counterPath(name + ".tmp")
		if err := os.WriteFile(tmp, []byte(strconv.FormatInt(n+1, 10)), 0o644); err != nil {
			return err
		}
		return os.Rename(tmp, s.counterPath(name))
	})
}

// Counter returns the current value, 0 when never bumped.
func (s *Store) Counter(name string) int64 {
	b, err := os.ReadFile(s.counterPath(name))
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return n
}

func (s *Store) SetServerPID(pid int) error {
	return os.WriteFile(s.path("server.pid"), []byte(strconv.Itoa(pid)), 0o644)
}
func (s *Store) ClearServerPID() { os.Remove(s.path("server.pid")) }

// ServerPID returns the live server pid, or 0 when there is none.
func (s *Store) ServerPID() int {
	b, err := os.ReadFile(s.path("server.pid"))
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return 0
	}
	return pid
}

func (s *Store) SetFlag(name, value string) error {
	if value == "" {
		value = "1"
	}
	return os.WriteFile(s.path(name), []byte(value), 0o644)
}
func (s *Store) Flag(name string) string {
	b, err := os.ReadFile(s.path(name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
func (s *Store) HasFlag(name string) bool { _, err := os.Stat(s.path(name)); return err == nil }
func (s *Store) ClearFlag(name string)    { os.Remove(s.path(name)) }

// The job lock serialises backups, update checks and restores, and the runner
// holds it from applying an update or restore until the relaunched server is
// ready. Jobs wait their turn rather than skip: the default schedules put the
// nightly backup and an hourly update check in the same second, and a skipped
// backup is invisible until the day it is needed.
var (
	ErrLockTimeout = errors.New("timed out waiting for the job lock")
	ErrStopping    = errors.New("the container is stopping")
)

// lockPoll is how often a waiting job retries the lock.
var lockPoll = time.Second

type LockOpts struct {
	Timeout time.Duration // 0 tries once
	Abort   func() bool   // checked before every attempt; true gives up with ErrStopping
	Waiting func()        // called once, when the lock first turns out to be held
}

// Held is a taken job lock. Release is idempotent and safe on nil.
type Held struct{ f *os.File }

func (h *Held) Release() {
	if h == nil || h.f == nil {
		return
	}
	syscall.Flock(int(h.f.Fd()), syscall.LOCK_UN)
	h.f.Close()
	h.f = nil
}

// Acquire takes the job lock, waiting up to o.Timeout for its holder.
func (s *Store) Acquire(o LockOpts) (*Held, error) {
	f, err := os.OpenFile(s.path("lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(o.Timeout)
	announced := false
	for {
		if o.Abort != nil && o.Abort() {
			f.Close()
			return nil, ErrStopping
		}
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case err == nil:
			return &Held{f: f}, nil
		case errors.Is(err, syscall.EINTR):
			continue
		case !errors.Is(err, syscall.EWOULDBLOCK):
			f.Close()
			return nil, fmt.Errorf("job lock: %w", err)
		}
		if !time.Now().Before(deadline) {
			f.Close()
			return nil, ErrLockTimeout
		}
		if !announced && o.Waiting != nil {
			o.Waiting()
			announced = true
		}
		time.Sleep(lockPoll)
	}
}

// Stopping reports whether the runner has been asked to stop.
func (s *Store) Stopping() bool { return s.HasFlag("stop.requested") }

// WaitLock blocks until the shared lock is free (used on the way down so a
// running backup finishes before the container exits).
func (s *Store) WaitLock() error {
	f, err := os.OpenFile(s.path("lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

// The tracked player set: one name per line, guarded by its own lock because
// the events consumer and any number of cron jobs touch it concurrently.
func (s *Store) withPlayers(fn func(names []string) ([]string, error)) error {
	lf, err := os.OpenFile(s.path("players.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)

	var names []string
	if f, err := os.Open(s.path("players")); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if line := sc.Text(); line != "" {
				names = append(names, line)
			}
		}
		f.Close()
	}
	out, err := fn(names)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	tmp := s.path("players.tmp")
	if err := os.WriteFile(tmp, []byte(strings.Join(out, "\n")+eol(out)), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path("players"))
}

func eol(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return "\n"
}

func (s *Store) PlayersAdd(name string) error {
	return s.withPlayers(func(names []string) ([]string, error) {
		for _, n := range names {
			if n == name {
				return nil, nil
			}
		}
		return append(names, name), nil
	})
}

func (s *Store) PlayersRemove(name string) error {
	return s.withPlayers(func(names []string) ([]string, error) {
		out := names[:0:0]
		for _, n := range names {
			if n != name {
				out = append(out, n)
			}
		}
		return out, nil
	})
}

func (s *Store) PlayersClear() error { return os.WriteFile(s.path("players"), nil, 0o644) }

func (s *Store) Players() []string {
	var out []string
	_ = s.withPlayers(func(names []string) ([]string, error) {
		out = append([]string(nil), names...)
		return nil, nil
	})
	sort.Strings(out)
	return out
}

func (s *Store) PlayersCount() int { return len(s.Players()) }

// ConsoleFIFO is the path of the server's stdin pipe.
func (s *Store) ConsoleFIFO() string { return s.path("console.fifo") }
