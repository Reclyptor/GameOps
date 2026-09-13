package metrics

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Reclyptor/GameOps/internal/backup"
	"github.com/Reclyptor/GameOps/internal/config"
	"github.com/Reclyptor/GameOps/internal/state"
)

func source(t *testing.T) *Source {
	t.Helper()
	st := state.New(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	bk := &backup.Backup{Cfg: &config.Config{BackupDir: t.TempDir()}, GameName: "fake"}
	return &Source{Game: "fake", St: st, Bk: bk, Health: func() error { return nil }}
}

func TestRender(t *testing.T) {
	ToolkitVersion = "1.0.0"
	s := source(t)
	s.St.SetFlag("server.version", `2.0 "beta"`)
	s.St.SetFlag("ready", "")
	s.St.PlayersAdd("Alice")
	s.St.PlayersAdd("Bob")
	s.St.SetFlag("update.available", "2.1")
	s.St.Bump(state.BackupsTotal)
	s.St.Bump(state.BackupsTotal)
	arch := filepath.Join(s.Bk.Cfg.BackupDir, "fake-2026-09-14_00-00-00.tar.gz")
	os.WriteFile(arch, []byte("0123456789"), 0o644)
	stamp := time.Unix(1_700_000_000, 0)
	os.Chtimes(arch, stamp, stamp)

	out := string(s.Render())
	for _, want := range []string{
		`gameops_info{game="fake",server_version="2.0 \"beta\"",toolkit_version="1.0.0"} 1`,
		"gameops_server_up 0",
		"gameops_server_ready 1",
		"gameops_players_online 2",
		"gameops_backup_last_timestamp_seconds 1700000000",
		"gameops_backup_last_size_bytes 10",
		"gameops_backup_archives 1",
		`gameops_update_pending{version="2.1"} 1`,
		"gameops_backups_total 2",
		"gameops_restores_total 0",
		"# TYPE gameops_backups_total counter",
		"# TYPE gameops_players_online gauge",
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("missing line %q in:\n%s", want, out)
		}
	}
	s.St.ClearFlag("update.available")
	if !strings.Contains(string(s.Render()), "gameops_update_pending 0\n") {
		t.Error("update_pending should be 0 without a label when nothing is pending")
	}
}

func TestCountersAcrossGoroutines(t *testing.T) {
	st := state.New(t.TempDir())
	st.Init()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				st.Bump(state.UpdatesTotal)
			}
		}()
	}
	wg.Wait()
	if n := st.Counter(state.UpdatesTotal); n != 200 {
		t.Fatalf("counter = %d, want 200", n)
	}
	if n := st.Counter("never"); n != 0 {
		t.Fatalf("unknown counter = %d", n)
	}
}

func TestServe(t *testing.T) {
	s := source(t)
	healthy := true
	s.Health = func() error {
		if healthy {
			return nil
		}
		return errors.New("server not ready")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, addr, s) }()

	get := func(path string) (int, string) {
		var resp *http.Response
		var err error
		for i := 0; i < 50; i++ {
			resp, err = http.Get(fmt.Sprintf("http://%s%s", addr, path))
			if err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	if code, body := get("/metrics"); code != 200 || !strings.Contains(body, "gameops_server_up 0") {
		t.Fatalf("/metrics → %d %q", code, body)
	}
	if code, body := get("/healthz"); code != 200 || strings.TrimSpace(body) != "ok" {
		t.Fatalf("/healthz → %d %q", code, body)
	}
	healthy = false
	if code, body := get("/healthz"); code != 503 || !strings.Contains(body, "not ready") {
		t.Fatalf("/healthz unhealthy → %d %q", code, body)
	}
	if code, _ := get("/anything"); code != 404 {
		t.Fatalf("unknown path → %d", code)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not stop")
	}
}
