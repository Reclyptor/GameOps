// Package metrics is the toolkit's only listener: a read-only HTTP endpoint
// serving Prometheus text on /metrics and the health check on /healthz.
// Everything it reports is read from the state directory and BACKUP_DIR at
// request time, so the cron jobs — separate processes — are visible too.
package metrics

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Reclyptor/GameOps/internal/backup"
	"github.com/Reclyptor/GameOps/internal/logx"
	"github.com/Reclyptor/GameOps/internal/state"
)

// ToolkitVersion is stamped by main from the build.
var ToolkitVersion = "dev"

var counterHelp = []struct{ name, help string }{
	{state.BackupsTotal, "Backups completed and verified."},
	{state.BackupFailuresTotal, "Backups that failed."},
	{state.UpdatesTotal, "Updates applied in place."},
	{state.RestoresTotal, "Restores applied."},
	{state.NotifyFailuresTotal, "Notifications that could not be delivered."},
	{state.ServerRestartsTotal, "Server relaunches inside this container (updates and restores)."},
	{state.GatePassedTotal, "Connections the TCP gate forwarded to the server."},
	{state.GateDroppedTotal, "Connections the TCP gate closed before they reached the server."},
}

type Source struct {
	Game string
	St   *state.Store
	Bk   *backup.Backup
	// Health returns nil when the server is healthy.
	Health func() error
}

// Render produces the Prometheus text exposition.
func (s *Source) Render() []byte {
	var b bytes.Buffer
	gauge := func(name, help string, value float64, labels ...string) {
		fmt.Fprintf(&b, "# HELP gameops_%s %s\n# TYPE gameops_%s gauge\n", name, help, name)
		fmt.Fprintf(&b, "gameops_%s%s %s\n", name, labelSet(labels), format(value))
	}
	counter := func(name, help string, value int64) {
		fmt.Fprintf(&b, "# HELP gameops_%s %s\n# TYPE gameops_%s counter\n", name, help, name)
		fmt.Fprintf(&b, "gameops_%s %d\n", name, value)
	}

	gauge("info", "Toolkit and server versions.", 1,
		"game", s.Game, "toolkit_version", ToolkitVersion, "server_version", s.St.Flag("server.version"))
	gauge("server_up", "1 when the server process is running.", boolf(s.St.ServerPID() != 0))
	gauge("server_ready", "1 once the server is joinable.", boolf(s.St.HasFlag("ready")))
	gauge("server_start_timestamp_seconds", "When the current server process was launched (0 when none).", float64(s.St.ServerStarted()))
	gauge("players_online", "Players currently online (from the tracked player set).", float64(s.St.PlayersCount()))

	var lastTS, lastSize float64
	var archives int
	if s.Bk != nil {
		if list, err := s.Bk.List(); err == nil {
			archives = len(list)
			if len(list) > 0 {
				lastTS = float64(list[0].ModTime.Unix())
				lastSize = float64(list[0].Size)
			}
		}
	}
	gauge("backup_last_timestamp_seconds", "Modification time of the newest archive (0 when none).", lastTS)
	gauge("backup_last_size_bytes", "Size of the newest archive.", lastSize)
	gauge("backup_archives", "Archives of this game in BACKUP_DIR.", float64(archives))

	if target := s.St.Flag("update.available"); target != "" {
		gauge("update_pending", "1 when a newer version is known and not yet applied.", 1, "version", target)
	} else {
		gauge("update_pending", "1 when a newer version is known and not yet applied.", 0)
	}
	for _, c := range counterHelp {
		counter(c.name, c.help, s.St.Counter(c.name))
	}
	return b.Bytes()
}

func boolf(v bool) float64 {
	if v {
		return 1
	}
	return 0
}

func format(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%g", v)
}

// labelSet renders key/value pairs as {k="v",...}, sorted, escaped.
func labelSet(kv []string) string {
	if len(kv) == 0 {
		return ""
	}
	pairs := make([]string, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		pairs = append(pairs, fmt.Sprintf("%s=%q", kv[i], escape(kv[i+1])))
	}
	sort.Strings(pairs)
	return "{" + strings.Join(pairs, ",") + "}"
}

func escape(v string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(v)
}

// Serve listens on addr until ctx is done. Only GET /metrics and GET /healthz
// exist; nothing is accepted or changed.
func Serve(ctx context.Context, addr string, src *Source) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Write(src.Render())
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if err := src.Health(); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintln(w, err)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()
	logx.Infof("metrics on http://%s/metrics (health on /healthz)", addr)
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
