package update

import "syscall"

func signalTerm(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }
