// Package logx is the toolkit's logger: timestamped lines on stderr so that
// values printed on stdout by adapter functions stay parseable.
package logx

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

type Level int

const (
	Debug Level = iota
	Info
	Warn
	Error
)

var (
	mu       sync.Mutex
	minLevel = Info
	out      = os.Stderr
)

// ParseLevel maps the LOG_LEVEL words to a Level; unknown words mean Info.
func ParseLevel(s string) (Level, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return Debug, true
	case "info", "":
		return Info, true
	case "warn", "warning":
		return Warn, true
	case "error":
		return Error, true
	}
	return Info, false
}

func SetLevel(l Level) { mu.Lock(); minLevel = l; mu.Unlock() }

func (l Level) String() string {
	switch l {
	case Debug:
		return "DEBUG"
	case Info:
		return "INFO"
	case Warn:
		return "WARN"
	default:
		return "ERROR"
	}
}

func logf(l Level, format string, args ...any) {
	mu.Lock()
	defer mu.Unlock()
	if l < minLevel {
		return
	}
	fmt.Fprintf(out, "%s [gameops] %-5s %s\n", time.Now().Format("2006-01-02 15:04:05"), l, fmt.Sprintf(format, args...))
}

func Debugf(format string, args ...any)  { logf(Debug, format, args...) }
func Infof(format string, args ...any)   { logf(Info, format, args...) }
func Warnf(format string, args ...any)   { logf(Warn, format, args...) }
func Errorf(format string, args ...any)  { logf(Error, format, args...) }
func Actionf(format string, args ...any) { logf(Info, "==> "+format, args...) }
