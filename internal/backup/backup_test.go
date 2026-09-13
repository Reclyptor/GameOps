package backup

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Reclyptor/GameOps/internal/config"
)

func entries(t *testing.T, path string) []string {
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
		io.Copy(io.Discard, tr)
	}
	return names
}

func TestWriteArchive(t *testing.T) {
	data := t.TempDir()
	os.MkdirAll(filepath.Join(data, "world", "sub"), 0o755)
	os.WriteFile(filepath.Join(data, "world", "a.txt"), []byte("alpha"), 0o644)
	os.WriteFile(filepath.Join(data, "world", "sub", "b.txt"), []byte("beta"), 0o644)
	os.WriteFile(filepath.Join(data, "config.json"), []byte("{}"), 0o644)
	b := &Backup{Cfg: &config.Config{DataDir: data, BackupDir: t.TempDir()}, GameName: "test"}
	dest := filepath.Join(b.Cfg.BackupDir, "x.tar.gz")
	if err := b.write(dest, []string{"world", "config.json"}); err != nil {
		t.Fatal(err)
	}
	got := entries(t, dest)
	want := map[string]bool{"world/": true, "world/a.txt": true, "world/sub/": true, "world/sub/b.txt": true, "config.json": true}
	for _, n := range got {
		delete(want, n)
	}
	if len(want) != 0 {
		t.Fatalf("missing entries %v in %v", want, got)
	}
}

func TestPrune(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "test-2026-01-01_00-00-00.tar.gz")
	recent := filepath.Join(dir, "test-2026-09-10_00-00-00.tar.gz")
	other := filepath.Join(dir, "other-2026-01-01_00-00-00.tar.gz")
	for _, p := range []string{old, recent, other} {
		os.WriteFile(p, nil, 0o644)
	}
	past := time.Now().AddDate(0, 0, -30)
	os.Chtimes(old, past, past)
	os.Chtimes(other, past, past)
	b := &Backup{Cfg: &config.Config{BackupDir: dir, BackupRetainDays: 14}, GameName: "test"}
	b.Prune()
	if _, err := os.Stat(old); err == nil {
		t.Error("old archive should be pruned")
	}
	if _, err := os.Stat(recent); err != nil {
		t.Error("recent archive should stay")
	}
	if _, err := os.Stat(other); err != nil {
		t.Error("other game's archive should stay")
	}
	b.Cfg.BackupRetainDays = 0
	os.WriteFile(old, nil, 0o644)
	os.Chtimes(old, past, past)
	b.Prune()
	if _, err := os.Stat(old); err != nil {
		t.Error("retain 0 keeps everything")
	}
}
