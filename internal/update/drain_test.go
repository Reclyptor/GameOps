package update

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Reclyptor/GameOps/internal/adapter"
	"github.com/Reclyptor/GameOps/internal/config"
	"github.com/Reclyptor/GameOps/internal/logx"
	"github.com/Reclyptor/GameOps/internal/state"
)

func TestBudgetMinutes(t *testing.T) {
	cases := map[int]int{0: 0, -1: 0, 1: 1, 59: 1, 60: 1, 300: 5, 330: 5, 1800: 30}
	for in, want := range cases {
		if got := BudgetMinutes(in); got != want {
			t.Errorf("BudgetMinutes(%d) = %d, want %d", in, got, want)
		}
	}
}

// drainer builds an Updater whose adapter cannot be executed, so Supports() is
// false and Call() fails — PlayerCount then falls back to the tracked player
// set, which is what these tests drive. That is also the real shape for a game
// with no game_players and no game_broadcast, such as Core Keeper.
func drainer(t *testing.T, sleep func(time.Duration)) (*Updater, *state.Store) {
	t.Helper()
	logx.SetLevel(logx.Error)
	dir := t.TempDir()
	st := state.New(dir)
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	return &Updater{
		Cfg:   &config.Config{UpdateWarnMinutes: 15, UpdateForceAfterMinutes: 30, StopTimeout: 120},
		St:    st,
		Ad:    adapter.New(filepath.Join(dir, "no-home"), filepath.Join(dir, "no-adapter")),
		Sleep: sleep,
	}, st
}

func TestDrainReturnsAtOnceWhenEmpty(t *testing.T) {
	slept := 0
	u, st := drainer(t, func(time.Duration) { slept++ })
	u.Drain(5)
	if slept != 0 {
		t.Errorf("slept %d times on an empty server, want 0", slept)
	}
	if st.Draining() {
		t.Error("drain flag left set after returning")
	}
}

func TestDrainWaitsOutTheBudgetWhenPlayersStay(t *testing.T) {
	slept := 0
	u, st := drainer(t, func(time.Duration) { slept++ })
	st.PlayersAdd("Alice")
	u.Drain(5)
	// No broadcast support, so the countdown waits for an empty server a minute
	// at a time and gives up at the budget rather than waiting forever.
	if slept != 5 {
		t.Errorf("slept %d times, want 5 (the budget)", slept)
	}
	if st.Draining() {
		t.Error("drain flag left set after returning")
	}
}

func TestDrainReturnsEarlyWhenTheServerEmpties(t *testing.T) {
	slept := 0
	var st *state.Store
	u, got := drainer(t, func(time.Duration) {
		slept++
		if slept == 2 {
			st.PlayersRemove("Alice")
		}
	})
	st = got
	st.PlayersAdd("Alice")
	u.Drain(30)
	if slept != 2 {
		t.Errorf("slept %d times, want 2 (left as soon as the server emptied)", slept)
	}
}

func TestDrainHoldsTheFlagWhileItWaits(t *testing.T) {
	var seen []bool
	var st *state.Store
	u, got := drainer(t, func(time.Duration) { seen = append(seen, st.Draining()) })
	st = got
	st.PlayersAdd("Alice")
	u.Drain(3)
	if len(seen) == 0 {
		t.Fatal("never waited, so the flag was never observable")
	}
	for i, up := range seen {
		if !up {
			t.Errorf("drain flag was clear during wait %d; a scheduled job could have started", i)
		}
	}
	if st.Draining() {
		t.Error("drain flag left set after returning")
	}
}

func TestDrainZeroBudgetDoesNotWait(t *testing.T) {
	slept := 0
	u, st := drainer(t, func(time.Duration) { slept++ })
	st.PlayersAdd("Alice")
	u.Drain(0)
	if slept != 0 {
		t.Errorf("slept %d times with a zero budget, want 0", slept)
	}
}
