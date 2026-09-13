// Package adapter bridges the toolkit to a game's adapter: a bash file of
// game_* functions (docs/CONTRACT.md §3). Functions run through the shim
// (shim/adapter-exec), which sources the helper library and the adapter and
// then calls the requested function. Exit code 2 means "unsupported".
package adapter

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const Unsupported = 2

type Adapter struct {
	Home string // GAMEOPS_HOME
	Path string // GAME_ADAPTER

	mu       sync.Mutex
	vars     map[string]string
	supports map[string]bool
}

func New(home, path string) *Adapter {
	return &Adapter{Home: home, Path: path, supports: map[string]bool{}}
}

func (a *Adapter) shim() string { return filepath.Join(a.Home, "shim", "adapter-exec") }

func (a *Adapter) command(args ...string) *exec.Cmd {
	cmd := exec.Command("bash", append([]string{a.shim()}, args...)...)
	cmd.Env = append(os.Environ(), "GAMEOPS_HOME="+a.Home, "GAME_ADAPTER="+a.Path)
	cmd.Stderr = os.Stderr
	return cmd
}

// Result of one adapter call.
type Result struct {
	Stdout string
	Code   int
}

func (r Result) OK() bool          { return r.Code == 0 }
func (r Result) Unsupported() bool { return r.Code == Unsupported }

// Call runs one adapter function and returns its trimmed stdout and exit code.
func (a *Adapter) Call(fn string, args ...string) (Result, error) {
	return a.CallTimeout(0, fn, args...)
}

// ErrTimeout is returned when a bounded call outlives its deadline.
var ErrTimeout = errors.New("adapter call timed out")

// CallTimeout is Call with a deadline; on expiry the whole process group
// (the shim, the adapter, anything it spawned such as steamcmd) is killed.
// A zero timeout means no deadline.
func (a *Adapter) CallTimeout(timeout time.Duration, fn string, args ...string) (Result, error) {
	cmd := a.command(append([]string{fn}, args...)...)
	var out bytes.Buffer
	cmd.Stdout = &out
	if timeout > 0 {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			return Result{}, fmt.Errorf("adapter %s: %w", fn, err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		var err error
		select {
		case err = <-done:
		case <-time.After(timeout):
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
			return Result{Code: 1}, fmt.Errorf("adapter %s: %w after %s", fn, ErrTimeout, timeout)
		}
		return result(out, err, fn)
	}
	err := cmd.Run()
	return result(out, err, fn)
}

func result(out bytes.Buffer, err error, fn string) (Result, error) {
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
			err = nil
		} else {
			return Result{}, fmt.Errorf("adapter %s: %w", fn, err)
		}
	}
	return Result{Stdout: strings.TrimRight(out.String(), "\n"), Code: code}, nil
}

// Lines runs a function and returns its non-empty stdout lines.
func (a *Adapter) Lines(fn string, args ...string) ([]string, Result, error) {
	r, err := a.Call(fn, args...)
	if err != nil {
		return nil, r, err
	}
	var lines []string
	for _, l := range strings.Split(r.Stdout, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return lines, r, nil
}

// Stream starts a long-running function (game_events) and returns its stdout
// and the process for the caller to wait on or kill.
func (a *Adapter) Stream(fn string) (*exec.Cmd, io.ReadCloser, error) {
	cmd := a.command(fn)
	cmd.Stdin = nil
	rc, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("adapter %s: %w", fn, err)
	}
	return cmd, rc, nil
}

// Supports reports whether the adapter overrides an optional function.
func (a *Adapter) Supports(fn string) bool {
	a.mu.Lock()
	if v, ok := a.supports[fn]; ok {
		a.mu.Unlock()
		return v
	}
	a.mu.Unlock()
	err := a.command("--supports", fn).Run()
	v := err == nil
	a.mu.Lock()
	a.supports[fn] = v
	a.mu.Unlock()
	return v
}

// Vars returns the variables the adapter sets at source time (GAME_NAME,
// GAME_DIR, GAME_PORT, GAME_PORT_PROTO, GAME_LOG, SERVER_NAME).
func (a *Adapter) Vars() (map[string]string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.vars != nil {
		return a.vars, nil
	}
	cmd := a.command("--vars")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("adapter did not load: %w", err)
	}
	vars := map[string]string{}
	sc := bufio.NewScanner(&out)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if ok {
			vars[k] = v
		}
	}
	for _, req := range []string{"GAME_NAME", "GAME_DIR"} {
		if vars[req] == "" {
			return nil, fmt.Errorf("adapter must set %s", req)
		}
	}
	a.vars = vars
	return vars, nil
}

func (a *Adapter) Var(name string) string {
	v, _ := a.Vars()
	return v[name]
}
