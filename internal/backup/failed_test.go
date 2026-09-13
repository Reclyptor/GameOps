package backup

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Reclyptor/GameOps/internal/config"
	"github.com/Reclyptor/GameOps/internal/notify"
	"github.com/Reclyptor/GameOps/internal/state"
)

// A backup that does not happen must be loud: posted, counted, and returned.
func TestFailedIsNeverSilent(t *testing.T) {
	bodies := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies <- string(b)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	t.Setenv("DISCORD_WEBHOOK_URL", srv.URL)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	st := state.New(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	b := &Backup{Cfg: cfg, Nt: notify.New(cfg, "Smoke"), St: st, GameName: "fake"}

	cause := state.ErrLockTimeout
	got := b.Failed(KindScheduled, cause)
	if !errors.Is(got, cause) {
		t.Fatalf("Failed must return the cause, got %v", got)
	}
	if n := st.Counter(state.BackupFailuresTotal); n != 1 {
		t.Fatalf("backup_failures_total = %d, want 1", n)
	}
	body := <-bodies
	for _, want := range []string{"Scheduled backup of the Smoke server failed", cause.Error()} {
		if !strings.Contains(body, want) {
			t.Errorf("BACKUP_FAILED post %q lacks %q", body, want)
		}
	}
}
