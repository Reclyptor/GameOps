// Package update detects updates and runs the player-aware countdown
// (docs/CONTRACT.md §4.3). The apply happens in the runner once the server
// has stopped.
package update

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Reclyptor/GameOps/internal/adapter"
	"github.com/Reclyptor/GameOps/internal/backup"
	"github.com/Reclyptor/GameOps/internal/config"
	"github.com/Reclyptor/GameOps/internal/logx"
	"github.com/Reclyptor/GameOps/internal/notify"
	"github.com/Reclyptor/GameOps/internal/state"
)

type Updater struct {
	Cfg *config.Config
	St  *state.Store
	Ad  *adapter.Adapter
	Nt  *notify.Notifier
	Bk  *backup.Backup
	// Sleep is replaceable for tests.
	Sleep func(time.Duration)
}

func (u *Updater) sleep(d time.Duration) {
	if u.Sleep != nil {
		u.Sleep(d)
		return
	}
	time.Sleep(d)
}

// CountdownMarks are the announcement points (minutes, descending) for a
// warning of n minutes: n itself, then 10, 5, 2, 1 — only those below n.
func CountdownMarks(n int) []int {
	if n <= 0 {
		return nil
	}
	marks := []int{n}
	for _, m := range []int{10, 5, 2, 1} {
		if m < n {
			marks = append(marks, m)
		}
	}
	return marks
}

// PlayerCount asks the adapter, falling back to the tracked set. Never fails.
func (u *Updater) PlayerCount() int {
	r, err := u.Ad.Call("game_players")
	if err == nil && r.OK() {
		if n, err := strconv.Atoi(strings.TrimSpace(r.Stdout)); err == nil && n >= 0 {
			return n
		}
	}
	return u.St.PlayersCount()
}

func (u *Updater) broadcast(msg string) {
	if r, err := u.Ad.Call("game_broadcast", msg); err != nil || !r.OK() {
		logx.Debugf("broadcast failed: %s", msg)
	}
}

// sleepWatching sleeps `minutes`, checking every minute; true when the
// server became empty.
func (u *Updater) sleepWatching(minutes int) bool {
	for i := 0; i < minutes; i++ {
		u.sleep(time.Minute)
		if u.PlayerCount() == 0 {
			return true
		}
	}
	return false
}

// RestartNote says, truthfully for this game and this moment, when the
// restart will happen: now (nobody online), after the in-game countdown, or
// once the server empties (no broadcast). It is what UPDATE_PRE and
// RESTORE_PRE tell the channel.
func (u *Updater) RestartNote() string {
	if u.PlayerCount() == 0 {
		return "restarting now"
	}
	if u.Ad.Supports("game_broadcast") {
		return fmt.Sprintf("restarting in %d minutes", u.Cfg.UpdateWarnMinutes)
	}
	return "restarting once the server is empty"
}

// Countdown returns once it is acceptable to restart the server, using the
// configured update patience. See CountdownWithin.
func (u *Updater) Countdown(reason string) {
	u.CountdownWithin(reason, u.Cfg.UpdateWarnMinutes, u.Cfg.UpdateForceAfterMinutes)
}

// CountdownWithin returns once it is acceptable to restart the server: at once
// when nobody is online, after the in-game countdown when the game can
// broadcast (reason completes "Server restarting … in N minutes"), otherwise
// after the server empties or forceAfterMinutes pass.
//
// The budget is a parameter rather than read from the configuration because a
// drain ahead of an eviction is deliberately less patient than a routine
// update: something wants the node back.
func (u *Updater) CountdownWithin(reason string, warnMinutes, forceAfterMinutes int) {
	if u.PlayerCount() == 0 {
		return
	}
	if u.Ad.Supports("game_broadcast") {
		marks := CountdownMarks(warnMinutes)
		for i, mark := range marks {
			next := 0
			if i+1 < len(marks) {
				next = marks[i+1]
			}
			if mark == 1 {
				u.broadcast(fmt.Sprintf("Server restarting %s in 1 minute. Please finish up.", reason))
				u.sleep(30 * time.Second)
				if u.PlayerCount() == 0 {
					return
				}
				u.broadcast(fmt.Sprintf("Server restarting %s in 30 seconds.", reason))
				u.sleep(20 * time.Second)
				u.broadcast(fmt.Sprintf("Server restarting %s in 10 seconds.", reason))
				u.sleep(10 * time.Second)
				return
			}
			u.broadcast(fmt.Sprintf("Server restarting %s in %d minutes.", reason, mark))
			if u.sleepWatching(mark - next) {
				return
			}
		}
		return
	}
	logx.Infof("players online and the game cannot broadcast; waiting up to %d min for an empty server", forceAfterMinutes)
	if u.sleepWatching(forceAfterMinutes) {
		return
	}
	logx.Warnf("server still not empty after %d min; restarting anyway", forceAfterMinutes)
}

// StopForRelaunch records why the server is being stopped (an update target
// or a restore archive) and asks it to stop; the runner reads the flag once
// the process is gone and relaunches in place. The flag is cleared again if
// the stop cannot even be requested.
func (u *Updater) StopForRelaunch(flag, value string) error {
	u.St.SetFlag(flag, value)
	sr, err := u.Ad.Call("game_shutdown")
	if err != nil {
		u.St.ClearFlag(flag)
		return err
	}
	if sr.Unsupported() {
		if pid := u.St.ServerPID(); pid > 0 {
			return signalTerm(pid)
		}
		u.St.ClearFlag(flag)
		return fmt.Errorf("no server to stop")
	}
	if !sr.OK() {
		u.St.ClearFlag(flag)
		return fmt.Errorf("game_shutdown failed (rc=%d); aborted", sr.Code)
	}
	return nil
}

// Check is the cron entry point body. Runs under the shared lock.
func (u *Updater) Check() error {
	if !u.Ad.Supports("game_update_available") {
		logx.Debugf("adapter does not support update checks")
		return nil
	}
	r, err := u.Ad.Call("game_update_available")
	if err != nil {
		return err
	}
	switch r.Code {
	case 0:
	case 1:
		v, _ := u.Ad.Call("game_version")
		logx.Debugf("server is current (%s)", v.Stdout)
		u.St.ClearFlag("update.available")
		return nil
	case adapter.Unsupported:
		return nil
	default:
		return fmt.Errorf("game_update_available failed (rc=%d)", r.Code)
	}
	target := strings.TrimSpace(r.Stdout)
	if target == "" {
		return fmt.Errorf("game_update_available returned 0 but printed no target")
	}
	u.St.SetFlag("update.available", target)
	if failed := u.St.Flag("update.failed"); failed == target {
		logx.Infof("update to %s failed earlier; not retrying until a different build appears or the container restarts", target)
		return nil
	}
	if u.St.ServerPID() == 0 {
		logx.Warnf("update to %s available but the server is not running; the runner applies updates on relaunch", target)
		return nil
	}
	cur, _ := u.Ad.Call("game_version")
	logx.Actionf("update available: %s → %s", cur.Stdout, target)
	if u.Cfg.UpdateSkipIfPlayers && u.PlayerCount() > 0 {
		logx.Infof("players online and UPDATE_SKIP_IF_PLAYERS=true; deferring update to %s", target)
		u.Nt.Send("UPDATE_DEFERRED", "version="+target)
		return nil
	}
	u.Nt.Send("UPDATE_PRE", "version="+target, "warn_minutes="+strconv.Itoa(u.Cfg.UpdateWarnMinutes), "restart_note="+u.RestartNote())
	u.Countdown("for an update")
	if u.Cfg.BackupOnUpdate {
		if err := u.Bk.Run(backup.KindPreUpdate); err != nil {
			logx.Warnf("pre-update backup failed; continuing with the update")
		}
	}
	logx.Infof("requesting server stop for update to %s", target)
	return u.StopForRelaunch("update.requested", target)
}

// OnBoot applies an available update before the first launch.
func (u *Updater) OnBoot() {
	if !u.Cfg.UpdateOnBoot || !u.Ad.Supports("game_update_available") {
		return
	}
	r, err := u.Ad.Call("game_update_available")
	if err != nil {
		logx.Warnf("boot update check failed: %v", err)
		return
	}
	switch r.Code {
	case 0:
		target := strings.TrimSpace(r.Stdout)
		if target == "" {
			return
		}
		cur, _ := u.Ad.Call("game_version")
		logx.Actionf("boot update: %s → %s", cur.Stdout, target)
		if ar, err := u.Ad.CallTimeout(time.Duration(u.Cfg.UpdateApplyTimeout)*time.Second, "game_update_apply", target); err == nil && ar.OK() {
			v, _ := u.Ad.Call("game_version")
			logx.Infof("updated to %s", v.Stdout)
		} else {
			logx.Errorf("boot update to %s failed; starting the installed version", target)
			u.St.SetFlag("update.failed", target)
			u.Nt.Send("UPDATE_FAILED", "version="+target, "reason=install failed")
		}
	case 1:
		v, _ := u.Ad.Call("game_version")
		logx.Infof("server is current (%s)", v.Stdout)
	default:
		logx.Warnf("boot update check failed (rc=%d); starting the installed version", r.Code)
	}
}
