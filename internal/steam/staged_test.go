package steam

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, p, content string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		return "<missing>"
	}
	return string(b)
}

func TestSwapInstallKeepsDataAndReplacesTheGame(t *testing.T) {
	dir := t.TempDir()
	// the live installation: old binaries, the world under Pal/Saved, a stale manifest
	write(t, filepath.Join(dir, "Pal", "Binaries", "server"), "old")
	write(t, filepath.Join(dir, "Pal", "Saved", "world.sav"), "precious")
	write(t, filepath.Join(dir, "steamapps", "appmanifest_1.acf"), "old-manifest")
	write(t, filepath.Join(dir, "steamapps", "downloading", "junk"), "x")
	write(t, filepath.Join(dir, "unrelated.txt"), "keep me")
	// the fresh download
	staging := filepath.Join(dir, ".steam-staging")
	write(t, filepath.Join(staging, "Pal", "Binaries", "server"), "new")
	write(t, filepath.Join(staging, "steamapps", "appmanifest_1.acf"), "new-manifest")

	if err := swapInstall(dir, staging, []string{"Pal/Saved"}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(dir, "Pal", "Binaries", "server")); got != "new" {
		t.Errorf("binary not replaced: %q", got)
	}
	if got := read(t, filepath.Join(dir, "Pal", "Saved", "world.sav")); got != "precious" {
		t.Errorf("world lost: %q", got)
	}
	if got := read(t, filepath.Join(dir, "steamapps", "appmanifest_1.acf")); got != "new-manifest" {
		t.Errorf("manifest not replaced: %q", got)
	}
	if exists(filepath.Join(dir, "steamapps", "downloading")) {
		t.Error("stale download state should be gone with the old steamapps")
	}
	if got := read(t, filepath.Join(dir, "unrelated.txt")); got != "keep me" {
		t.Errorf("entries the fresh install lacks must stay: %q", got)
	}
	for _, d := range []string{".steam-staging", ".steam-old"} {
		if exists(filepath.Join(dir, d)) {
			t.Errorf("%s left behind", d)
		}
	}
}

func TestSwapInstallRollsBack(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	write(t, filepath.Join(dir, "Pal", "Binaries", "server"), "old")
	write(t, filepath.Join(dir, "Pal", "Saved", "world.sav"), "precious")
	staging := filepath.Join(dir, ".steam-staging")
	write(t, filepath.Join(staging, "Pal", "Binaries", "server"), "new")
	os.MkdirAll(filepath.Join(dir, ".steam-old"), 0o755)
	// a read-only dir makes the first rename fail
	os.Chmod(dir, 0o555)
	defer os.Chmod(dir, 0o755)
	if err := swapInstall(dir, staging, []string{"Pal/Saved"}); err == nil {
		t.Fatal("expected the swap to fail")
	}
	os.Chmod(dir, 0o755)
	if got := read(t, filepath.Join(dir, "Pal", "Binaries", "server")); got != "old" {
		t.Errorf("live installation changed by a failed swap: %q", got)
	}
	if got := read(t, filepath.Join(dir, "Pal", "Saved", "world.sav")); got != "precious" {
		t.Errorf("world touched by a failed swap: %q", got)
	}
}
