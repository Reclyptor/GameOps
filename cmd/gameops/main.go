// gameops is the operations layer for Reclyptor game-server images: a
// supervised lifecycle, backups, in-place updates, notifications, player
// events, a console pipe and a health check, plus helper subcommands for
// adapters. See docs/CONTRACT.md.
package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Reclyptor/GameOps/internal/adapter"
	"github.com/Reclyptor/GameOps/internal/backup"
	"github.com/Reclyptor/GameOps/internal/config"
	"github.com/Reclyptor/GameOps/internal/console"
	"github.com/Reclyptor/GameOps/internal/health"
	"github.com/Reclyptor/GameOps/internal/httpx"
	"github.com/Reclyptor/GameOps/internal/jsonx"
	"github.com/Reclyptor/GameOps/internal/logx"
	"github.com/Reclyptor/GameOps/internal/metrics"
	"github.com/Reclyptor/GameOps/internal/notify"
	"github.com/Reclyptor/GameOps/internal/rcon"
	"github.com/Reclyptor/GameOps/internal/restore"
	"github.com/Reclyptor/GameOps/internal/runner"
	"github.com/Reclyptor/GameOps/internal/state"
	"github.com/Reclyptor/GameOps/internal/steam"
	"github.com/Reclyptor/GameOps/internal/update"
)

// Version is set at build time from the git tag.
var Version = "dev"

const usage = `usage: gameops <command> [args]

lifecycle   run | backup [list | verify [archive|latest]] | restore <archive|latest> [--no-backup]
            update | health | notify <EVENT> [key=value ...] | console <line>
adapters    rcon <command...>            http get [-o file] <url>
            json get <file|-> <path>     json set <file> <path> <value> [--raw]
            json escape <string>         json array [items...]
            steam install [--keep path]... <dir> <appid...>   steam update-check <dir> <appid> <depot>
            players                      wait-settled <path> [quiet-seconds] [timeout-seconds]
            tcp-open <host> <port>       version
`

func main() {
	metrics.ToolkitVersion = Version
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(64)
	}
	if os.Args[1] == "run" && runner.IsInit() {
		os.Exit(runner.RunAsInit(os.Args[1:]))
	}
	os.Exit(dispatch(os.Args[1], os.Args[2:]))
}

// app is the assembled toolkit for the commands that need the adapter.
type app struct {
	cfg  *config.Config
	st   *state.Store
	ad   *adapter.Adapter
	nt   *notify.Notifier
	vars map[string]string
	bk   *backup.Backup
	up   *update.Updater
}

func boot() (*app, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	if lvl, ok := logx.ParseLevel(cfg.LogLevel); ok {
		logx.SetLevel(lvl)
	}
	ad := adapter.New(cfg.Home, cfg.Adapter)
	vars, err := ad.Vars()
	if err != nil {
		return nil, err
	}
	st := state.New(cfg.State)
	nt := notify.New(cfg, vars["SERVER_NAME"])
	nt.OnFailure = func() { st.Bump(state.NotifyFailuresTotal) }
	bk := &backup.Backup{Cfg: cfg, Ad: ad, Nt: nt, St: st, GameName: vars["GAME_NAME"]}
	up := &update.Updater{Cfg: cfg, St: st, Ad: ad, Nt: nt, Bk: bk}
	return &app{cfg: cfg, st: st, ad: ad, nt: nt, vars: vars, bk: bk, up: up}, nil
}

func fail(err error) int {
	logx.Errorf("%v", err)
	return 1
}

func dispatch(cmd string, args []string) int {
	switch cmd {
	case "version":
		fmt.Println(Version)
		return 0
	case "run":
		return cmdRun()
	case "backup":
		return cmdBackup(args)
	case "restore":
		return cmdRestore(args)
	case "update":
		return cmdLocked("update", func(a *app) error { return a.up.Check() })
	case "drain":
		return cmdDrain(args)
	case "health":
		return cmdHealth()
	case "notify":
		if len(args) < 1 {
			fmt.Fprintln(os.Stderr, "usage: gameops notify <EVENT> [key=value ...]")
			return 64
		}
		a, err := boot()
		if err != nil {
			return fail(err)
		}
		a.nt.Send(args[0], args[1:]...)
		return 0
	case "console":
		if len(args) < 1 {
			fmt.Fprintln(os.Stderr, "usage: gameops console <line>")
			return 64
		}
		cfg, err := config.Load()
		if err != nil {
			return fail(err)
		}
		if err := console.Send(state.New(cfg.State).ConsoleFIFO(), strings.Join(args, " ")); err != nil {
			return fail(err)
		}
		return 0
	case "players":
		cfg, err := config.Load()
		if err != nil {
			return fail(err)
		}
		fmt.Println(state.New(cfg.State).PlayersCount())
		return 0
	case "rcon":
		return cmdRcon(args)
	case "http":
		return cmdHTTP(args)
	case "json":
		return cmdJSON(args)
	case "steam":
		return cmdSteam(args)
	case "wait-settled":
		return cmdWaitSettled(args)
	case "tcp-open":
		if len(args) != 2 {
			return 64
		}
		c, err := net.DialTimeout("tcp", net.JoinHostPort(args[0], args[1]), 3*time.Second)
		if err != nil {
			return 1
		}
		c.Close()
		return 0
	default:
		fmt.Fprint(os.Stderr, usage)
		return 64
	}
}

func cmdRun() int {
	a, err := boot()
	if err != nil {
		return fail(err)
	}
	logx.Infof("gameops %s starting (uid %d, gid %d, TZ %s)", Version, os.Getuid(), os.Getgid(), envOr("TZ", "UTC"))
	runner.Report(a.ad, a.vars)
	if err := a.st.Init(); err != nil {
		return fail(err)
	}
	if err := runner.Boot(a.cfg, a.ad, a.vars, a.up); err != nil {
		return fail(err)
	}
	return runner.New(a.cfg, a.st, a.ad, a.nt, a.vars).Main()
}

// acquireJobLock takes the job lock for job, waiting behind whatever job or
// relaunch holds it. It gives up after LOCK_TIMEOUT, or at once when the
// container is stopping, and never lets job start in either case.
func acquireJobLock(a *app, job string) (*state.Held, error) {
	h, err := a.st.Acquire(state.LockOpts{
		Timeout: time.Duration(a.cfg.LockTimeout) * time.Second,
		Abort:   a.st.Quiescing,
		Waiting: func() {
			logx.Infof("%s: another job or a relaunch holds the job lock; waiting up to %ds", job, a.cfg.LockTimeout)
		},
	})
	if errors.Is(err, state.ErrLockTimeout) {
		err = fmt.Errorf("not started: %w after %ds", err, a.cfg.LockTimeout)
	} else if err != nil {
		err = fmt.Errorf("not started: %w", err)
	}
	return h, err
}

// cmdDrain is the orchestrator's pre-stop hook: it blocks until stopping the
// server is acceptable, then exits 0. It deliberately does NOT take the job
// lock — a backup already running should finish while players are being warned,
// and the stop path waits for it. Drain only stops *new* jobs starting.
//
// --required-grace prints the minimum terminationGracePeriodSeconds for the
// budget instead of draining, so a manifest and this binary cannot drift.
func cmdDrain(args []string) int {
	const usage = "usage: gameops drain [--deadline <seconds>] [--required-grace]"
	deadline, haveDeadline, printGrace := 0, false, false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--required-grace":
			printGrace = true
		case "--deadline":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, usage)
				return 64
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil || n < 0 {
				fmt.Fprintf(os.Stderr, "drain: --deadline wants a non-negative integer, got %q\n", args[i+1])
				return 64
			}
			deadline, haveDeadline = n, true
			i++
		default:
			fmt.Fprintf(os.Stderr, "drain: unknown argument %q\n%s\n", args[i], usage)
			return 64
		}
	}
	a, err := boot()
	if err != nil {
		return fail(err)
	}
	if !haveDeadline {
		deadline = max(a.cfg.UpdateWarnMinutes, a.cfg.UpdateForceAfterMinutes) * 60
	}
	if printGrace {
		fmt.Println(a.cfg.RequiredGrace(deadline))
		return 0
	}
	a.up.Drain(update.BudgetMinutes(deadline))
	return 0
}

// cmdLocked runs fn under the job lock. A job that cannot run exits non-zero:
// silently skipping one is how a missing nightly backup goes unnoticed.
func cmdLocked(job string, fn func(*app) error) int {
	a, err := boot()
	if err != nil {
		return fail(err)
	}
	h, err := acquireJobLock(a, job)
	if errors.Is(err, state.ErrStopping) {
		logx.Warnf("%s %v", job, err)
		return 1
	}
	if err != nil {
		return fail(fmt.Errorf("%s %w", job, err))
	}
	defer h.Release()
	if err := fn(a); err != nil {
		return fail(err)
	}
	return 0
}

func cmdBackup(args []string) int {
	if len(args) == 0 {
		kind := backup.KindManual
		if os.Getenv("GAMEOPS_TRIGGER") == "schedule" {
			kind = backup.KindScheduled
		}
		a, err := boot()
		if err != nil {
			return fail(err)
		}
		h, err := acquireJobLock(a, "backup")
		if errors.Is(err, state.ErrStopping) {
			// The shutdown saves the world itself; a backup that never started
			// on a stopping container is not a failed backup.
			logx.Warnf("backup %v", err)
			return 1
		}
		if err != nil {
			// A backup that never got its turn is a missed backup.
			a.bk.Failed(kind, err)
			return 1
		}
		defer h.Release()
		if err := a.bk.Run(kind); err != nil {
			return 1 // Run reports its own failures through Failed
		}
		return 0
	}
	switch args[0] {
	case "list":
		a, err := boot()
		if err != nil {
			return fail(err)
		}
		archives, err := a.bk.List()
		if err != nil {
			return fail(err)
		}
		for _, ar := range archives {
			fmt.Printf("%s\t%s\t%s ago\n", ar.Path, backup.Human(ar.Size), time.Since(ar.ModTime).Round(time.Minute))
		}
		return 0
	case "verify":
		spec := "latest"
		if len(args) > 1 {
			spec = args[1]
		}
		a, err := boot()
		if err != nil {
			return fail(err)
		}
		archive, err := a.bk.Resolve(spec)
		if err != nil {
			return fail(err)
		}
		// The adapter's paths that exist right now are what a restore must
		// bring back; an archive missing one of them is reported, not hidden.
		paths, _, err := a.ad.Lines("game_backup_paths")
		if err != nil {
			return fail(err)
		}
		var required []string
		for _, p := range paths {
			if _, err := os.Lstat(a.cfg.DataDir + "/" + p); err == nil {
				required = append(required, p)
			}
		}
		rep, err := backup.Verify(archive, required)
		if err != nil {
			fmt.Fprintf(os.Stderr, "FAILED: %s — %v\n", archive, err)
			return 1
		}
		fmt.Printf("OK: %s — %d files, %s (paths: %s)\n", archive, rep.Entries, backup.Human(rep.Bytes), strings.Join(rep.Top, " "))
		return 0
	}
	fmt.Fprintln(os.Stderr, "usage: gameops backup [list | verify [archive|latest]]")
	return 64
}

func cmdRestore(args []string) int {
	safety := true
	spec := ""
	for _, a := range args {
		switch {
		case a == "--no-backup":
			safety = false
		case strings.HasPrefix(a, "-"):
			fmt.Fprintln(os.Stderr, "usage: gameops restore <archive|latest> [--no-backup]")
			return 64
		default:
			spec = a
		}
	}
	if spec == "" {
		fmt.Fprintln(os.Stderr, "usage: gameops restore <archive|latest> [--no-backup]")
		return 64
	}
	return cmdLocked("restore", func(a *app) error {
		r := &restore.Restorer{Cfg: a.cfg, St: a.st, Ad: a.ad, Nt: a.nt, Bk: a.bk, Up: a.up}
		return r.Run(spec, safety)
	})
}

func cmdHealth() int {
	a, err := boot()
	if err != nil {
		return fail(err)
	}
	if err := health.Check(a.st, a.ad, a.vars); err != nil {
		fmt.Fprintln(os.Stderr, "unhealthy:", err)
		return 1
	}
	return 0
}

func cmdRcon(args []string) int {
	addr := ""
	pass := os.Getenv("RCON_PASSWORD")
	var rest []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-a":
			i++
			if i < len(args) {
				addr = args[i]
			}
		case "-p":
			i++
			if i < len(args) {
				pass = args[i]
			}
		default:
			rest = append(rest, args[i])
		}
	}
	if addr == "" {
		port := os.Getenv("RCON_PORT")
		if port == "" {
			fmt.Fprintln(os.Stderr, "rcon: RCON_PORT (or -a host:port) is required")
			return 64
		}
		addr = "127.0.0.1:" + port
	}
	if pass == "" {
		fmt.Fprintln(os.Stderr, "rcon: RCON_PASSWORD (or -p) is required")
		return 64
	}
	if len(rest) == 0 {
		fmt.Fprintln(os.Stderr, "usage: gameops rcon [-a host:port] [-p password] <command...>")
		return 64
	}
	out, err := rcon.Command(addr, pass, strings.Join(rest, " "), 10*time.Second)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rcon:", err)
		return 1
	}
	if out != "" {
		fmt.Println(out)
	}
	return 0
}

func cmdHTTP(args []string) int {
	if len(args) < 2 || args[0] != "get" {
		fmt.Fprintln(os.Stderr, "usage: gameops http get [-o file] <url>")
		return 64
	}
	out := ""
	url := ""
	for i := 1; i < len(args); i++ {
		if args[i] == "-o" && i+1 < len(args) {
			out = args[i+1]
			i++
			continue
		}
		url = args[i]
	}
	if url == "" {
		return 64
	}
	if out != "" {
		if err := httpx.Download(url, out); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	b, err := httpx.Get(url)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	os.Stdout.Write(b)
	return 0
}

func readInput(name string) ([]byte, error) {
	if name == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(name)
}

func cmdJSON(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: gameops json get|set|escape|array ...")
		return 64
	}
	switch args[0] {
	case "get":
		if len(args) != 3 {
			fmt.Fprintln(os.Stderr, "usage: gameops json get <file|-> <path>")
			return 64
		}
		data, err := readInput(args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		v, err := jsonx.Get(data, args[2])
		if errors.Is(err, jsonx.ErrMissing) {
			return 1
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		fmt.Println(v)
		return 0
	case "set":
		raw := false
		var rest []string
		for _, a := range args[1:] {
			if a == "--raw" {
				raw = true
			} else {
				rest = append(rest, a)
			}
		}
		if len(rest) != 3 {
			fmt.Fprintln(os.Stderr, "usage: gameops json set <file> <path> <value> [--raw]")
			return 64
		}
		data, err := os.ReadFile(rest[0])
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		out, err := jsonx.Set(data, rest[1], rest[2], raw)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		tmp := rest[0] + ".tmp"
		if err := os.WriteFile(tmp, out, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if err := os.Rename(tmp, rest[0]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	case "escape":
		fmt.Println(jsonx.Escape(strings.Join(args[1:], " ")))
		return 0
	case "array":
		fmt.Println(jsonx.Array(args[1:]))
		return 0
	}
	fmt.Fprintln(os.Stderr, "usage: gameops json get|set|escape|array ...")
	return 64
}

func cmdSteam(args []string) int {
	steamcmd := envOr("STEAMCMD_DIR", "/home/steam/steamcmd")
	if len(args) >= 1 && args[0] == "install" {
		var keep, rest []string
		for i := 1; i < len(args); i++ {
			if args[i] == "--keep" && i+1 < len(args) {
				keep = append(keep, args[i+1])
				i++
				continue
			}
			rest = append(rest, args[i])
		}
		if len(rest) < 2 {
			fmt.Fprintln(os.Stderr, "usage: gameops steam install [--keep path]... <dir> <appid...>")
			return 64
		}
		stall, _ := strconv.Atoi(envOr("STALL_SECONDS", "300"))
		if err := steam.Update(steamcmd, rest[0], rest[1:], time.Duration(stall)*time.Second, keep); err != nil {
			logx.Errorf("steam install failed: %v", err)
			return 1
		}
		return 0
	}
	if len(args) == 4 && args[0] == "update-check" {
		remote, avail, err := steam.UpdateAvailable(args[1], args[2], args[3])
		if err != nil {
			logx.Warnf("steam update check: %v", err)
			return 3
		}
		if avail {
			fmt.Println(remote)
			return 0
		}
		return 1
	}
	fmt.Fprintln(os.Stderr, "usage: gameops steam install [--keep path]... <dir> <appid...> | steam update-check <dir> <appid> <depot>")
	return 64
}

func cmdWaitSettled(args []string) int {
	if len(args) < 1 {
		return 64
	}
	quiet, timeout := 5, 300
	if len(args) > 1 {
		quiet, _ = strconv.Atoi(args[1])
	}
	if len(args) > 2 {
		timeout, _ = strconv.Atoi(args[2])
	}
	path := args[0]
	last := newestMtime(path)
	for waited := 0; waited < timeout; waited += quiet {
		time.Sleep(time.Duration(quiet) * time.Second)
		now := newestMtime(path)
		if now == last {
			return 0
		}
		last = now
	}
	return 1
}

func newestMtime(path string) int64 {
	var newest int64
	var walk func(p string)
	walk = func(p string) {
		fi, err := os.Lstat(p)
		if err != nil {
			return
		}
		if t := fi.ModTime().UnixNano(); t > newest {
			newest = t
		}
		if fi.IsDir() {
			entries, _ := os.ReadDir(p)
			for _, e := range entries {
				walk(p + "/" + e.Name())
			}
		}
	}
	walk(path)
	return newest
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

var _ = syscall.Kill
