package backup

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Archive is one of this game's archives in BACKUP_DIR.
type Archive struct {
	Path    string
	Size    int64
	ModTime time.Time
}

// List returns this game's archives, newest first.
func (b *Backup) List() ([]Archive, error) {
	entries, err := os.ReadDir(b.Cfg.BackupDir)
	if err != nil {
		return nil, err
	}
	var out []Archive
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || strings.HasPrefix(n, ".") || !strings.HasPrefix(n, b.GameName+"-") || !strings.HasSuffix(n, ".tar.gz") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, Archive{Path: filepath.Join(b.Cfg.BackupDir, n), Size: info.Size(), ModTime: info.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].ModTime.Equal(out[j].ModTime) {
			return out[i].ModTime.After(out[j].ModTime)
		}
		return out[i].Path > out[j].Path
	})
	return out, nil
}

// Resolve turns what an operator typed into an archive path: "latest" (or
// nothing), a file name inside BACKUP_DIR, or a path.
func (b *Backup) Resolve(spec string) (string, error) {
	if spec == "" || spec == "latest" {
		archives, err := b.List()
		if err != nil {
			return "", err
		}
		if len(archives) == 0 {
			return "", fmt.Errorf("no %s archives in %s", b.GameName, b.Cfg.BackupDir)
		}
		return archives[0].Path, nil
	}
	if !strings.ContainsRune(spec, '/') {
		if p := filepath.Join(b.Cfg.BackupDir, spec); exists(p) {
			return p, nil
		}
	}
	if exists(spec) {
		return spec, nil
	}
	return "", fmt.Errorf("archive not found: %s", spec)
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Report is what Verify learned about an archive.
type Report struct {
	Entries int
	Bytes   int64    // uncompressed
	Top     []string // top-level paths, sorted
}

// Verify reads an archive end to end — the gzip checksum and every tar
// entry — and checks that each required DataDir-relative path is in it.
// An archive that passes is one a restore can be built from.
func Verify(archive string, required []string) (Report, error) {
	var rep Report
	f, err := os.Open(archive)
	if err != nil {
		return rep, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return rep, fmt.Errorf("not a gzip archive: %w", err)
	}
	tr := tar.NewReader(gz)
	names := map[string]bool{}
	top := map[string]bool{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return rep, fmt.Errorf("corrupt archive at entry %d: %w", rep.Entries+1, err)
		}
		if err := safeName(h.Name); err != nil {
			return rep, err
		}
		n, err := io.Copy(io.Discard, tr)
		if err != nil {
			return rep, fmt.Errorf("corrupt entry %s: %w", h.Name, err)
		}
		rep.Entries++
		rep.Bytes += n
		clean := path.Clean(h.Name)
		names[clean] = true
		top[strings.SplitN(clean, "/", 2)[0]] = true
	}
	// The tar stream ends before the gzip trailer; draining the rest is what
	// makes the reader check the stored CRC.
	if _, err := io.Copy(io.Discard, gz); err != nil {
		return rep, fmt.Errorf("corrupt archive: %w", err)
	}
	for p := range top {
		rep.Top = append(rep.Top, p)
	}
	sort.Strings(rep.Top)
	var missing []string
	for _, r := range required {
		if r = path.Clean(strings.TrimSpace(r)); r != "" && r != "." && !names[r] {
			missing = append(missing, r)
		}
	}
	if len(missing) > 0 {
		return rep, fmt.Errorf("archive lacks %s", strings.Join(missing, ", "))
	}
	return rep, nil
}

// safeName rejects entry names that could write outside the extraction root.
func safeName(name string) error {
	if name == "" || strings.HasPrefix(name, "/") {
		return fmt.Errorf("unsafe entry name %q", name)
	}
	clean := path.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, "../") || clean == "." {
		return fmt.Errorf("unsafe entry name %q", name)
	}
	if strings.HasPrefix(clean, ".restore-") {
		return fmt.Errorf("entry name %q collides with the restore staging area", name)
	}
	return nil
}

// Restore replaces the archive's top-level paths under dataDir with the
// archive's contents. Everything is extracted into a staging directory first;
// the live paths are swapped by rename only once the whole archive is on
// disk, and swapped back if any rename fails, so a failed restore leaves the
// data exactly as it was.
func Restore(dataDir, archive string) (Report, error) {
	rep, err := Verify(archive, nil)
	if err != nil {
		return rep, err
	}
	staging := filepath.Join(dataDir, ".restore-staging")
	old := filepath.Join(dataDir, ".restore-old")
	os.RemoveAll(staging)
	os.RemoveAll(old)
	if err := extract(archive, staging); err != nil {
		os.RemoveAll(staging)
		return rep, err
	}
	if err := os.MkdirAll(old, 0o755); err != nil {
		os.RemoveAll(staging)
		return rep, err
	}

	var swapped []string
	rollback := func(cause error) error {
		for _, p := range swapped {
			live := filepath.Join(dataDir, p)
			os.RemoveAll(live)
			if prev := filepath.Join(old, p); exists(prev) {
				os.Rename(prev, live)
			}
		}
		os.RemoveAll(staging)
		os.RemoveAll(old)
		return fmt.Errorf("restore aborted, data unchanged: %w", cause)
	}
	for _, p := range rep.Top {
		live := filepath.Join(dataDir, p)
		if exists(live) {
			if err := os.Rename(live, filepath.Join(old, p)); err != nil {
				return rep, rollback(err)
			}
		}
		swapped = append(swapped, p)
		if err := os.Rename(filepath.Join(staging, p), live); err != nil {
			return rep, rollback(err)
		}
	}
	os.RemoveAll(old)
	os.RemoveAll(staging)
	return rep, nil
}

// extract unpacks archive under root, restoring modes and mtimes.
func extract(archive, root string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	type stamp struct {
		path string
		mod  time.Time
	}
	var dirs []stamp
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if err := safeName(h.Name); err != nil {
			return err
		}
		dest := filepath.Join(root, filepath.FromSlash(path.Clean(h.Name)))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dest, os.FileMode(h.Mode)|0o700); err != nil {
				return err
			}
			dirs = append(dirs, stamp{dest, h.ModTime})
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(h.Mode)|0o600)
			if err != nil {
				return err
			}
			_, err = io.Copy(out, tr)
			if cerr := out.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return fmt.Errorf("%s: %w", h.Name, err)
			}
			os.Chtimes(dest, h.ModTime, h.ModTime)
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(h.Linkname, dest); err != nil {
				return err
			}
		default:
			// Devices, fifos and the like have no place in game data.
		}
	}
	if _, err := io.Copy(io.Discard, gz); err != nil {
		return err
	}
	// Children were written after their directories; stamp the directories last.
	for i := len(dirs) - 1; i >= 0; i-- {
		os.Chtimes(dirs[i].path, dirs[i].mod, dirs[i].mod)
	}
	return nil
}
