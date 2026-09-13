package runner

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

const childEnv = "GAMEOPS_INIT_CHILD"

// IsInit reports whether this process should act as the container's init:
// it is PID 1 and not already the re-exec'd child.
func IsInit() bool { return os.Getpid() == 1 && os.Getenv(childEnv) == "" }

// RunAsInit is a minimal init: re-exec this binary as a child with the same
// arguments, forward termination signals to it, reap every orphan, and exit
// with the child's status. Nothing else lives in PID 1.
func RunAsInit(args []string) int {
	child := exec.Command("/proc/self/exe", args...)
	child.Env = append(os.Environ(), childEnv+"=1")
	child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := child.Start(); err != nil {
		os.Stderr.WriteString("gameops init: cannot start child: " + err.Error() + "\n")
		return 1
	}
	pid := child.Process.Pid

	sigs := make(chan os.Signal, 16)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGUSR1, syscall.SIGUSR2, syscall.SIGCHLD)
	for sig := range sigs {
		if sig != syscall.SIGCHLD {
			syscall.Kill(pid, sig.(syscall.Signal))
			continue
		}
		for {
			var ws syscall.WaitStatus
			reaped, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
			if err != nil || reaped <= 0 {
				break
			}
			if reaped == pid {
				if ws.Signaled() {
					return 128 + int(ws.Signal())
				}
				return ws.ExitStatus()
			}
		}
	}
	return 1
}
