// Package runner is the supervisor (docs/CONTRACT.md §4): launch the server,
// wait for readiness, watch it, stop it gracefully, relaunch after an update.
package runner

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Reclyptor/GameOps/internal/adapter"
	"github.com/Reclyptor/GameOps/internal/backup"
	"github.com/Reclyptor/GameOps/internal/config"
	"github.com/Reclyptor/GameOps/internal/console"
	"github.com/Reclyptor/GameOps/internal/cron"
	"github.com/Reclyptor/GameOps/internal/gate"
	"github.com/Reclyptor/GameOps/internal/health"
	"github.com/Reclyptor/GameOps/internal/logx"
	"github.com/Reclyptor/GameOps/internal/metrics"
	"github.com/Reclyptor/GameOps/internal/notify"
	"github.com/Reclyptor/GameOps/internal/state"
	"github.com/Reclyptor/GameOps/internal/update"
)

type Runner struct {
	Cfg  *config.Config
	St   *state.Store
	Ad   *adapter.Adapter
	Nt   *notify.Notifier
	Vars map[string]string

	stopOnce sync.Once
	stopCh   chan struct{}

	// relaunch is the job lock taken for an update or restore; it is carried
	// into the next cycle and released once the relaunched server is ready.
	relaunch *state.Held
}

func New(cfg *config.Config, st *state.Store, ad *adapter.Adapter, nt *notify.Notifier, vars map[string]string) *Runner {
	return &Runner{Cfg: cfg, St: st, Ad: ad, Nt: nt, Vars: vars, stopCh: make(chan struct{})}
}

func (r *Runner) requestStop() {
	r.stopOnce.Do(func() {
		logx.Actionf("stop requested")
		r.St.SetFlag("stop.requested", "")
		close(r.stopCh)
	})
}

func (r *Runner) stopRequested() bool {
	select {
	case <-r.stopCh:
		return true
	default:
		return false
	}
}

// Main runs the supervisor loop and returns the process exit code.
func (r *Runner) Main() int {
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigs
		r.requestStop()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.startCron(ctx)
	r.startMetrics(ctx)
	r.startGate(ctx)
	defer r.waitForJobs()

	for {
		code, again := r.cycle()
		if !again {
			return code
		}
	}
}

// cycle launches the server once and decides what happens after it exits.
func (r *Runner) cycle() (code int, again bool) {
	held := r.relaunch
	r.relaunch = nil
	defer held.Release()
	defer func() {
		if !again { // nothing will relaunch, so nothing may keep the lock
			r.relaunch.Release()
			r.relaunch = nil
		}
	}()

	con, err := console.Open(r.St.ConsoleFIFO())
	if err != nil {
		logx.Errorf("%v", err)
		return 1, false
	}
	defer con.Close()

	cmd, logFile, err := r.launch(con)
	if err != nil {
		logx.Errorf("%v", err)
		r.Nt.Send("CRASH", "reason=server process did not start")
		return 1, false
	}
	defer logFile.Close()
	pid := cmd.Process.Pid
	r.St.SetServerPID(pid)
	r.St.SetServerStarted()
	if v, err := r.Ad.Call("game_version"); err == nil && v.OK() {
		r.St.SetFlag("server.version", v.Stdout)
	}
	defer r.St.ClearServerPID()
	defer r.St.ClearFlag("ready")

	done := make(chan struct{})
	var waitErr error
	go func() { waitErr = cmd.Wait(); close(done) }()

	events := r.startEvents()
	defer r.stopEvents(events)

	if r.waitReady(done) {
		r.St.SetFlag("ready", "")
		held.Release()
		logx.Infof("%s is ready", r.Vars["SERVER_NAME"])
		r.Nt.Send("START", r.connectionDetails()...)
	} else if !r.stopRequested() {
		select {
		case <-done:
		default:
			syscall.Kill(pid, syscall.SIGTERM)
			<-done
		}
		r.Nt.Send("CRASH", "reason=not ready within "+itoa(r.Cfg.ReadyTimeout)+"s")
		return 1, false
	}

	stopping := false
	var killTimer *time.Timer
	for {
		if r.stopRequested() && !stopping {
			stopping = true
			r.Nt.Send("STOP")
			killTimer = r.stopServer(pid)
		}
		select {
		case <-done:
			if killTimer != nil {
				killTimer.Stop()
			}
			rc := exitCode(cmd, waitErr)
			return r.afterExit(rc)
		case <-r.stopCh:
			continue
		}
	}
}

func itoa(n int) string { return strings.TrimSpace(strings.Repeat(" ", 0) + itoaFast(n)) }

func itoaFast(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func (r *Runner) afterExit(rc int) (int, bool) {
	if r.stopRequested() {
		logx.Infof("server stopped (rc=%d)", rc)
		return 0, false
	}
	if target := r.St.Flag("update.requested"); target != "" {
		r.St.ClearFlag("update.requested")
		r.holdForRelaunch()
		logx.Actionf("applying update to %s", target)
		res, err := r.Ad.CallTimeout(time.Duration(r.Cfg.UpdateApplyTimeout)*time.Second, "game_update_apply", target)
		if err == nil && res.OK() {
			v, _ := r.Ad.Call("game_version")
			logx.Infof("updated to %s; relaunching", v.Stdout)
			r.St.ClearFlag("update.available")
			r.St.Bump(state.UpdatesTotal)
			r.St.Bump(state.ServerRestartsTotal)
			r.Nt.Send("UPDATE_POST", "version="+v.Stdout)
			return 0, true
		}
		// The installed version is intact (adapters stage, then swap), so the
		// server comes back on it; the target is remembered so the hourly
		// check does not stop the server for the same failing build again.
		reason := "install failed"
		if errors.Is(err, adapter.ErrTimeout) {
			reason = fmt.Sprintf("install still running after %ds, killed", r.Cfg.UpdateApplyTimeout)
		}
		logx.Errorf("update to %s failed (%s); relaunching the installed version", target, reason)
		r.St.SetFlag("update.failed", target)
		r.St.Bump(state.ServerRestartsTotal)
		r.Nt.Send("UPDATE_FAILED", "version="+target, "reason="+reason)
		return 0, true
	}
	if archive := r.St.Flag("restore.requested"); archive != "" {
		r.St.ClearFlag("restore.requested")
		r.holdForRelaunch()
		logx.Actionf("restoring %s", archive)
		rep, err := backup.Restore(r.Cfg.DataDir, archive)
		if err != nil {
			logx.Errorf("restore from %s failed: %v", archive, err)
			r.Nt.Send("RESTORE_FAILED", "file_path="+archive, "reason="+err.Error())
			return 1, false
		}
		logx.Infof("restored %d files from %s; relaunching", rep.Entries, archive)
		r.St.Bump(state.RestoresTotal)
		r.St.Bump(state.ServerRestartsTotal)
		r.Nt.Send("RESTORE_POST", "file_path="+archive)
		return 0, true
	}
	if rc == 0 {
		logx.Infof("server exited cleanly on its own")
		r.Nt.Send("STOP")
		return 0, false
	}
	logx.Errorf("server exited unexpectedly (rc=%d)", rc)
	r.Nt.Send("CRASH", "reason=exit code "+itoa(rc))
	return rc, false
}

// connectionDetails asks the adapter how players actually reach this server,
// for the START message: the address they connect to and the password it
// enforces. Either is the truth where the game differs from what was
// configured — a password it made up itself, or an address its client can
// use. Adapters that do not answer keep announcing SERVER_ADDRESS and
// GAME_PASSWORD, and a failure is a warning, never a failed start.
func (r *Runner) connectionDetails() []string {
	var kv []string
	for _, d := range []struct{ fn, key, env string }{
		{"game_server_address", "server_address", "SERVER_ADDRESS"},
		{"game_join_password", "game_password", "GAME_PASSWORD"},
	} {
		res, err := r.Ad.Call(d.fn)
		switch {
		case err != nil:
			logx.Warnf("%s: %v; announcing %s", d.fn, err, d.env)
		case res.Unsupported():
		case !res.OK():
			logx.Warnf("%s failed (rc=%d); announcing %s", d.fn, res.Code, d.env)
		default:
			kv = append(kv, d.key+"="+res.Stdout)
		}
	}
	return kv
}

// holdForRelaunch takes the job lock for an apply or swap and the relaunch
// after it, so no backup, update check or restore touches a server that is
// not back yet. The job that asked for the relaunch lets go of the lock as
// soon as it has asked; a backup that got in first finishes against the
// stopped server, which is a consistent one. Never blocks the relaunch
// itself: without the lock it goes ahead unguarded and says so.
func (r *Runner) holdForRelaunch() {
	h, err := r.St.Acquire(state.LockOpts{
		Timeout: time.Duration(r.Cfg.LockTimeout) * time.Second,
		Waiting: func() { logx.Infof("waiting for the job lock before relaunching") },
	})
	if err != nil {
		logx.Warnf("relaunching without the job lock: %v", err)
		return
	}
	r.relaunch = h
}

// launch starts the server with stdin from the console FIFO and output
// copied to the console log and container stdout.
func (r *Runner) launch(con *console.Console) (*exec.Cmd, *os.File, error) {
	argv, err := r.startCmd()
	if err != nil {
		return nil, nil, err
	}
	logPath := r.Vars["GAME_LOG"]
	os.MkdirAll(filepath.Dir(logPath), 0o755)
	if _, err := os.Stat(logPath); err == nil {
		os.Rename(logPath, strings.TrimSuffix(logPath, ".log")+".prev.log")
	}
	logFile, err := os.Create(logPath)
	if err != nil {
		return nil, nil, err
	}
	logx.Actionf("starting %s: %s", r.Vars["GAME_NAME"], strings.Join(argv, " "))
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = r.Vars["GAME_DIR"]
	cmd.Stdin = con.Stdin()
	out := io.MultiWriter(os.Stdout, logFile)
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, nil, err
	}
	return cmd, logFile, nil
}

func (r *Runner) startCmd() ([]string, error) {
	res, err := r.Ad.Call("--start-cmd")
	if err != nil {
		return nil, err
	}
	if !res.OK() {
		return nil, errors.New("game_start_cmd failed")
	}
	var argv []string
	for _, a := range strings.Split(res.Stdout, "\x00") {
		if a != "" {
			argv = append(argv, a)
		}
	}
	if len(argv) == 0 {
		return nil, errors.New("game_start_cmd produced an empty GAME_CMD")
	}
	return argv, nil
}

func (r *Runner) waitReady(done <-chan struct{}) bool {
	deadline := time.Now().Add(time.Duration(r.Cfg.ReadyTimeout) * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			return false
		default:
		}
		if r.stopRequested() {
			return false
		}
		if res, err := r.Ad.Call("game_ready"); err == nil && res.OK() {
			return true
		}
		select {
		case <-done:
			return false
		case <-r.stopCh:
			return false
		case <-time.After(5 * time.Second):
		}
	}
	logx.Errorf("server not ready after %ds", r.Cfg.ReadyTimeout)
	return false
}

// stopServer asks the server to stop and arms a SIGKILL for STOP_TIMEOUT later.
func (r *Runner) stopServer(pid int) *time.Timer {
	res, err := r.Ad.Call("game_shutdown")
	if err != nil || res.Unsupported() || !res.OK() {
		if err == nil && !res.Unsupported() {
			logx.Warnf("game_shutdown failed (rc=%d); sending SIGTERM", res.Code)
		}
		syscall.Kill(pid, syscall.SIGTERM)
	}
	return time.AfterFunc(time.Duration(r.Cfg.StopTimeout)*time.Second, func() {
		if syscall.Kill(pid, 0) == nil {
			logx.Warnf("server ignored shutdown for %ds; killing", r.Cfg.StopTimeout)
			syscall.Kill(pid, syscall.SIGKILL)
		}
	})
}

func exitCode(cmd *exec.Cmd, err error) int {
	if err == nil {
		return 0
	}
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok {
		if ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ws.ExitStatus()
	}
	return 1
}

// ── scheduled jobs ─────────────────────────────────────────────────────────

func (r *Runner) startCron(ctx context.Context) {
	type job struct {
		name string
		expr string
	}
	var jobs []job
	if r.Cfg.BackupEnabled {
		jobs = append(jobs, job{"backup", r.Cfg.BackupCron})
	}
	if r.Cfg.UpdateEnabled && r.Ad.Supports("game_update_available") {
		jobs = append(jobs, job{"update", r.Cfg.UpdateCron})
	}
	if len(jobs) == 0 {
		logx.Infof("no scheduled jobs (backups and updates disabled or unsupported)")
		return
	}
	var desc []string
	for _, j := range jobs {
		desc = append(desc, j.expr+" "+j.name)
	}
	logx.Infof("schedule: %s", strings.Join(desc, "; "))
	for _, j := range jobs {
		sched, err := cron.Parse(j.expr)
		if err != nil {
			logx.Errorf("%s schedule invalid: %v", j.name, err)
			continue
		}
		go func(name string, sched *cron.Schedule) {
			for {
				next := sched.Next(time.Now())
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Until(next)):
				}
				logx.Debugf("cron: running %s", name)
				cmd := exec.Command("/proc/self/exe", name)
				cmd.Env = append(os.Environ(), "GAMEOPS_TRIGGER=schedule")
				cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
				if err := cmd.Run(); err != nil {
					logx.Warnf("cron: %s failed: %v", name, err)
				}
			}
		}(j.name, sched)
	}
}

// startGate fronts the game's port for the life of the runner (§4.7).
func (r *Runner) startGate(ctx context.Context) {
	if !r.Cfg.GateEnabled {
		return
	}
	g := &gate.Gate{
		Listen:  ":" + itoa(r.Cfg.GatePort),
		Target:  "127.0.0.1:" + itoa(r.Cfg.GateTargetPort),
		Expect:  r.Cfg.GateExpect,
		Timeout: time.Duration(r.Cfg.GateTimeoutSeconds) * time.Second,
		OnPass:  func() { r.St.Bump(state.GatePassedTotal) },
		OnDrop:  func() { r.St.Bump(state.GateDroppedTotal) },
	}
	go func() {
		if err := g.Serve(ctx); err != nil {
			logx.Errorf("gate: %v", err)
		}
	}()
}

// startMetrics serves /metrics and /healthz for the life of the runner.
func (r *Runner) startMetrics(ctx context.Context) {
	if r.Cfg.MetricsPort == 0 {
		logx.Infof("metrics listener disabled (METRICS_PORT=0)")
		return
	}
	src := &metrics.Source{
		Game:   r.Vars["GAME_NAME"],
		St:     r.St,
		Bk:     &backup.Backup{Cfg: r.Cfg, GameName: r.Vars["GAME_NAME"]},
		Health: func() error { return health.Check(r.St, r.Ad, r.Vars) },
	}
	go func() {
		if err := metrics.Serve(ctx, ":"+itoa(r.Cfg.MetricsPort), src); err != nil {
			logx.Errorf("metrics listener: %v", err)
		}
	}()
}

// waitForJobs gives a running backup or update a chance to finish before the
// container exits (bounded).
func (r *Runner) waitForJobs() {
	done := make(chan struct{})
	go func() { r.St.WaitLock(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Minute):
		logx.Warnf("gave up waiting for a running job to finish")
	}
}

// ── player events ──────────────────────────────────────────────────────────

func (r *Runner) startEvents() *exec.Cmd {
	if !r.Cfg.PlayerEventsEnabled || !r.Ad.Supports("game_events") {
		return nil
	}
	r.St.PlayersClear()
	cmd, out, err := r.Ad.Stream("game_events")
	if err != nil {
		logx.Warnf("could not start game_events: %v", err)
		return nil
	}
	go func() {
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			kind, name, _ := strings.Cut(sc.Text(), " ")
			name = strings.TrimSpace(name)
			switch kind {
			case "JOIN":
				if name == "" {
					continue
				}
				r.St.PlayersAdd(name)
				logx.Infof("player joined: %s (%d online)", name, r.St.PlayersCount())
				r.Nt.Send("JOIN", "player_name="+name)
			case "LEAVE":
				if name == "" {
					continue
				}
				r.St.PlayersRemove(name)
				logx.Infof("player left: %s (%d online)", name, r.St.PlayersCount())
				r.Nt.Send("LEAVE", "player_name="+name)
			default:
				logx.Debugf("ignoring event line: %s", sc.Text())
			}
		}
		cmd.Wait()
	}()
	return cmd
}

func (r *Runner) stopEvents(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	cmd.Process.Kill()
	r.St.PlayersClear()
}

// Boot runs the pre-launch sequence: writable checks, install, update on boot.
func Boot(cfg *config.Config, ad *adapter.Adapter, vars map[string]string, up *update.Updater) error {
	if err := config.RequireWritable(cfg.DataDir, "DATA_DIR"); err != nil {
		return err
	}
	if cfg.BackupEnabled {
		if err := config.RequireWritable(cfg.BackupDir, "BACKUP_DIR"); err != nil {
			return err
		}
	}
	if ad.Supports("game_update_apply") {
		if err := config.RequireWritable(vars["GAME_DIR"], "GAME_DIR (updates replace the installation in place)"); err != nil {
			return err
		}
	}
	logx.Actionf("install check")
	if res, err := ad.Call("game_install"); err != nil || !res.OK() {
		return errors.New("game_install failed")
	}
	if v, err := ad.Call("game_version"); err == nil && v.OK() && v.Stdout != "" {
		logx.Infof("installed version: %s", v.Stdout)
	} else {
		logx.Infof("installed version: not available until first start")
	}
	up.OnBoot()
	return nil
}

// Report logs, once at boot, what the adapter can and cannot do.
func Report(ad *adapter.Adapter, vars map[string]string) {
	var caps []string
	for _, fn := range []string{"game_update_available", "game_update_apply", "game_save", "game_broadcast",
		"game_players", "game_events", "game_backup_begin", "game_healthy", "game_shutdown"} {
		if ad.Supports(fn) {
			caps = append(caps, strings.TrimPrefix(fn, "game_"))
		}
	}
	c := "none"
	if len(caps) > 0 {
		c = strings.Join(caps, " ")
	}
	logx.Infof("adapter %s: %s (overrides: %s)", vars["GAME_NAME"], vars["GAME_DIR"], c)
	if !ad.Supports("game_events") && !ad.Supports("game_players") {
		logx.Warnf("adapter reports no player events and no player query: player count is always 0, update countdowns will not wait for players")
	}
}
