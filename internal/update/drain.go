package update

import "github.com/Reclyptor/GameOps/internal/logx"

// DrainReason completes the countdown's "Server restarting … in N minutes".
const DrainReason = "for maintenance"

// BudgetMinutes converts a drain deadline in seconds into whole countdown
// minutes. A sub-minute budget rounds UP to one rather than down to zero, so a
// small deadline still warns players; an explicit zero means "do not wait" and
// stays zero.
func BudgetMinutes(deadlineSeconds int) int {
	if deadlineSeconds <= 0 {
		return 0
	}
	if m := deadlineSeconds / 60; m > 0 {
		return m
	}
	return 1
}

// Drain blocks until stopping the server is acceptable, then returns. It is the
// orchestrator's pre-stop hook: Kubernetes runs it, waits for it, and only then
// sends SIGTERM, so an eviction takes the same countdown an in-place update
// takes instead of dropping players without warning.
//
// budgetMinutes bounds the wait absolutely. That bound is the safety property:
// a pre-stop hook runs inside the orchestrator's grace period, so a drain still
// counting down when the period expires is SIGKILLed and the save never
// happens — strictly worse than not draining at all. Config.RequiredGrace
// reports the grace period a given budget needs.
//
// The drain flag is held for the duration so no new scheduled job starts behind
// it. A job that is already running keeps the lock and is left to finish; the
// stop path waits for it. The flag is cleared on return, because a drain that
// ends without a stop — a cancelled eviction — must leave the scheduler working.
func (u *Updater) Drain(budgetMinutes int) {
	if err := u.St.SetDraining(); err != nil {
		// Not fatal: the countdown is still worth running. It only means a
		// scheduled job could start behind us.
		logx.Warnf("drain: cannot record the drain flag: %v", err)
	}
	defer u.St.ClearDraining()

	if n := u.PlayerCount(); n == 0 {
		logx.Infof("drain: nobody online; stopping now")
		return
	}
	logx.Infof("drain: %d player(s) online; %s", u.PlayerCount(), u.RestartNote())
	u.CountdownWithin(DrainReason, budgetMinutes, budgetMinutes)
}
