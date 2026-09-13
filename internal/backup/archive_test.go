package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Reclyptor/GameOps/internal/config"
)

// fixture writes a small data tree and archives world + config.json.
func fixture(t *testing.T) (*Backup, string) {
	t.Helper()
	data := t.TempDir()
	os.MkdirAll(filepath.Join(data, "world", "sub"), 0o755)
	os.WriteFile(filepath.Join(data, "world", "a.txt"), []byte("alpha"), 0o644)
	os.WriteFile(filepath.Join(data, "world", "sub", "b.txt"), []byte("beta"), 0o600)
	os.WriteFile(filepath.Join(data, "config.json"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(data, "untouched.txt"), []byte("keep"), 0o644)
	b := &Backup{Cfg: &config.Config{DataDir: data, BackupDir: t.TempDir()}, GameName: "test"}
	archive := filepath.Join(b.Cfg.BackupDir, "test-2026-09-14_01-00-00.tar.gz")
	if err := b.write(archive, []string{"world", "config.json"}); err != nil {
		t.Fatal(err)
	}
	return b, archive
}

func TestVerify(t *testing.T) {
	b, archive := fixture(t)
	rep, err := Verify(archive, []string{"world", "config.json"})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Entries != 5 || rep.Bytes != int64(len("alpha")+len("beta")+len("{}")) {
		t.Fatalf("report %+v", rep)
	}
	if strings.Join(rep.Top, " ") != "config.json world" {
		t.Fatalf("top-level paths %v", rep.Top)
	}
	if _, err := Verify(archive, []string{"world", "mods"}); err == nil || !strings.Contains(err.Error(), "mods") {
		t.Fatalf("missing path should be named, got %v", err)
	}

	// A truncated file fails on the gzip stream, not silently.
	raw, _ := os.ReadFile(archive)
	trunc := filepath.Join(b.Cfg.BackupDir, "test-trunc.tar.gz")
	os.WriteFile(trunc, raw[:len(raw)-20], 0o644)
	if _, err := Verify(trunc, nil); err == nil {
		t.Error("truncated archive should fail verification")
	}
	// A flipped byte inside the compressed stream fails the CRC.
	flipped := append([]byte(nil), raw...)
	flipped[len(flipped)/2] ^= 0xff
	bad := filepath.Join(b.Cfg.BackupDir, "test-bad.tar.gz")
	os.WriteFile(bad, flipped, 0o644)
	if _, err := Verify(bad, nil); err == nil {
		t.Error("corrupted archive should fail verification")
	}
	if _, err := Verify(filepath.Join(b.Cfg.BackupDir, "missing.tar.gz"), nil); err == nil {
		t.Error("missing archive should fail")
	}
}

func hostileArchive(t *testing.T, dir, name string) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("x")
	tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
	tw.Write(body)
	tw.Close()
	gz.Close()
	p := filepath.Join(dir, "hostile.tar.gz")
	os.WriteFile(p, buf.Bytes(), 0o644)
	return p
}

func TestVerifyRejectsUnsafeNames(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"../escape", "/abs", "a/../../b", ".restore-old/x"} {
		if _, err := Verify(hostileArchive(t, dir, name), nil); err == nil {
			t.Errorf("%q should be rejected", name)
		}
	}
}

func TestRestore(t *testing.T) {
	b, archive := fixture(t)
	data := b.Cfg.DataDir
	// Damage the live data in every way a restore must undo: changed content,
	// an extra file, a removed file.
	os.WriteFile(filepath.Join(data, "world", "a.txt"), []byte("tampered"), 0o644)
	os.WriteFile(filepath.Join(data, "world", "extra.txt"), []byte("junk"), 0o644)
	os.Remove(filepath.Join(data, "config.json"))

	rep, err := Restore(data, archive)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Entries != 5 {
		t.Fatalf("report %+v", rep)
	}
	if got, _ := os.ReadFile(filepath.Join(data, "world", "a.txt")); string(got) != "alpha" {
		t.Errorf("a.txt not restored: %q", got)
	}
	if _, err := os.Stat(filepath.Join(data, "world", "extra.txt")); err == nil {
		t.Error("extra file should be gone: the archive's tree replaces the live one")
	}
	if got, _ := os.ReadFile(filepath.Join(data, "config.json")); string(got) != "{}" {
		t.Errorf("config.json not restored: %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(data, "untouched.txt")); string(got) != "keep" {
		t.Error("paths outside the archive must not be touched")
	}
	if st, _ := os.Stat(filepath.Join(data, "world", "sub", "b.txt")); st.Mode().Perm() != 0o600 {
		t.Errorf("mode not restored: %v", st.Mode())
	}
	for _, d := range []string{".restore-staging", ".restore-old"} {
		if _, err := os.Stat(filepath.Join(data, d)); err == nil {
			t.Errorf("%s left behind", d)
		}
	}
}

func TestRestoreRollsBack(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions; the rename cannot be made to fail")
	}
	b, archive := fixture(t)
	data := b.Cfg.DataDir
	os.WriteFile(filepath.Join(data, "world", "a.txt"), []byte("live"), 0o644)
	// Staging succeeds (the staging dir is created before the lock-down),
	// then the swap fails on a read-only data dir and must leave the live
	// tree exactly as it was.
	os.MkdirAll(filepath.Join(data, ".restore-staging"), 0o755)
	os.MkdirAll(filepath.Join(data, ".restore-old"), 0o755)
	os.Chmod(data, 0o555)
	defer os.Chmod(data, 0o755)
	if _, err := Restore(data, archive); err == nil {
		t.Fatal("expected the swap to fail")
	}
	os.Chmod(data, 0o755)
	if got, _ := os.ReadFile(filepath.Join(data, "world", "a.txt")); string(got) != "live" {
		t.Errorf("live data changed by a failed restore: %q", got)
	}
}

func TestArchivePathNeverCollides(t *testing.T) {
	b := &Backup{Cfg: &config.Config{BackupDir: t.TempDir()}, GameName: "test"}
	first := b.archivePath()
	os.WriteFile(first, []byte("x"), 0o644)
	second := b.archivePath()
	if second == first {
		t.Fatal("a second archive in the same second must get its own name")
	}
	os.WriteFile(second, []byte("x"), 0o644)
	if third := b.archivePath(); third == first || third == second {
		t.Fatalf("third name collides: %s", third)
	}
	list, _ := b.List()
	if len(list) != 2 {
		t.Fatalf("both archives should list: %v", list)
	}
}

func TestListResolve(t *testing.T) {
	b := &Backup{Cfg: &config.Config{BackupDir: t.TempDir()}, GameName: "test"}
	if _, err := b.Resolve("latest"); err == nil {
		t.Error("no archives: latest must fail")
	}
	older := filepath.Join(b.Cfg.BackupDir, "test-2026-09-01_00-00-00.tar.gz")
	newer := filepath.Join(b.Cfg.BackupDir, "test-2026-09-02_00-00-00.tar.gz")
	for _, p := range []string{older, newer, filepath.Join(b.Cfg.BackupDir, "other-2026-09-03_00-00-00.tar.gz"), filepath.Join(b.Cfg.BackupDir, ".test-partial.tar.gz.partial")} {
		os.WriteFile(p, []byte("x"), 0o644)
	}
	past := time.Now().Add(-time.Hour)
	os.Chtimes(older, past, past)
	list, err := b.List()
	if err != nil || len(list) != 2 || list[0].Path != newer || list[1].Path != older {
		t.Fatalf("list %v, %v", list, err)
	}
	if p, _ := b.Resolve("latest"); p != newer {
		t.Errorf("latest → %s", p)
	}
	if p, _ := b.Resolve(""); p != newer {
		t.Errorf("empty → %s", p)
	}
	if p, _ := b.Resolve(filepath.Base(older)); p != older {
		t.Errorf("basename → %s", p)
	}
	if p, _ := b.Resolve(older); p != older {
		t.Errorf("path → %s", p)
	}
	if _, err := b.Resolve("nope.tar.gz"); err == nil {
		t.Error("unknown name must fail")
	}
}
