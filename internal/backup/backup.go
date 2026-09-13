// Package backup implements one backup cycle (docs/CONTRACT.md §4.4, §5):
// quiesce → save → archive → verify → prune → notify. Archives are written
// in process (archive/tar + gzip), read back before they count, renamed into
// place atomically, and guarded against files that change mid-read.
package backup

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Reclyptor/GameOps/internal/adapter"
	"github.com/Reclyptor/GameOps/internal/config"
	"github.com/Reclyptor/GameOps/internal/logx"
	"github.com/Reclyptor/GameOps/internal/notify"
	"github.com/Reclyptor/GameOps/internal/state"
)

type Backup struct {
	Cfg      *config.Config
	Ad       *adapter.Adapter
	Nt       *notify.Notifier
	St       *state.Store // optional: counters for /metrics
	GameName string
}

func (b *Backup) bump(name string) {
	if b.St != nil {
		b.St.Bump(name)
	}
}

// ErrChanged reports a file that changed while being archived.
var ErrChanged = errors.New("file changed while archiving")

// archivePath names the next archive by the second, with a counter when
// that second already has one: a restore's safety backup can land in the
// same second as the archive being restored and must never overwrite it.
func (b *Backup) archivePath() string {
	stem := filepath.Join(b.Cfg.BackupDir, fmt.Sprintf("%s-%s", b.GameName, time.Now().Format("2006-01-02_15-04-05")))
	p := stem + ".tar.gz"
	for n := 2; exists(p); n++ {
		p = fmt.Sprintf("%s-%d.tar.gz", stem, n)
	}
	return p
}

// Kinds of backup, named in the notifications so a reader can tell the
// nightly run from the one an update or restore takes first.
const (
	KindScheduled  = "Scheduled"
	KindManual     = "Manual"
	KindPreUpdate  = "Pre-update"
	KindPreRestore = "Pre-restore"
)

// Run performs one cycle of the given kind. Callers hold the shared lock.
func (b *Backup) Run(kind string) error {
	if err := config.RequireWritable(b.Cfg.BackupDir, "BACKUP_DIR"); err != nil {
		return err
	}
	paths, _, err := b.Ad.Lines("game_backup_paths")
	if err != nil {
		return err
	}
	var present []string
	for _, p := range paths {
		if _, err := os.Lstat(filepath.Join(b.Cfg.DataDir, p)); err == nil {
			present = append(present, p)
		} else {
			logx.Warnf("backup path missing, skipping: %s", filepath.Join(b.Cfg.DataDir, p))
		}
	}
	if len(present) == 0 {
		logx.Errorf("nothing to back up: none of the adapter's paths exist under %s", b.Cfg.DataDir)
		b.bump(state.BackupFailuresTotal)
		b.Nt.Send("BACKUP_FAILED", "backup_kind="+kind, "reason=no backup paths exist")
		return errors.New("no backup paths exist")
	}

	logx.Actionf("%s backup: %s", strings.ToLower(kind), strings.Join(present, " "))
	b.Nt.Send("BACKUP_PRE", "backup_kind="+kind)

	if r, _ := b.Ad.Call("game_backup_begin"); r.Code != 0 && !r.Unsupported() {
		logx.Warnf("game_backup_begin failed; continuing")
	}
	if r, _ := b.Ad.Call("game_save"); r.OK() {
		logx.Infof("world saved")
	} else if r.Unsupported() {
		logx.Debugf("game has no save command; relying on its own autosave")
	} else {
		logx.Warnf("game_save failed (rc=%d); archiving whatever is on disk", r.Code)
	}

	final := b.archivePath()
	tmp := filepath.Join(b.Cfg.BackupDir, "."+filepath.Base(final)+".partial")
	err = b.write(tmp, present)
	if errors.Is(err, ErrChanged) {
		logx.Warnf("files changed while archiving; settling for %ds and retrying", b.Cfg.BackupSettleSeconds)
		time.Sleep(time.Duration(b.Cfg.BackupSettleSeconds) * time.Second)
		err = b.write(tmp, present)
	}
	if r, _ := b.Ad.Call("game_backup_end"); r.Code != 0 && !r.Unsupported() {
		logx.Warnf("game_backup_end failed")
	}
	if err != nil {
		os.Remove(tmp)
		logx.Errorf("archive failed: %v", err)
		b.bump(state.BackupFailuresTotal)
		b.Nt.Send("BACKUP_FAILED", "backup_kind="+kind, "reason="+err.Error())
		return err
	}
	// An archive counts only once it has been read back in full.
	rep, err := Verify(tmp, present)
	if err != nil {
		os.Remove(tmp)
		logx.Errorf("archive failed verification: %v", err)
		b.bump(state.BackupFailuresTotal)
		b.Nt.Send("BACKUP_FAILED", "backup_kind="+kind, "reason=verification failed: "+err.Error())
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		return err
	}
	if st, err := os.Stat(final); err == nil {
		logx.Infof("backup written: %s (%s)", final, Human(st.Size()))
	}
	logx.Infof("backup verified: %d files, %s", rep.Entries, Human(rep.Bytes))
	b.bump(state.BackupsTotal)
	b.Prune()
	b.Nt.Send("BACKUP_POST", "backup_kind="+kind, "file_path="+final)
	return nil
}

// write archives the given DataDir-relative paths into dest.
func (b *Backup) write(dest string, paths []string) error {
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	werr := func() error {
		for _, p := range paths {
			if err := addPath(tw, b.Cfg.DataDir, p); err != nil {
				return err
			}
		}
		return nil
	}()
	if err := tw.Close(); err != nil && werr == nil {
		werr = err
	}
	if err := gz.Close(); err != nil && werr == nil {
		werr = err
	}
	if err := f.Close(); err != nil && werr == nil {
		werr = err
	}
	return werr
}

func addPath(tw *tar.Writer, root, rel string) error {
	base := filepath.Join(root, rel)
	return filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		name, _ := filepath.Rel(root, path)
		name = filepath.ToSlash(name)
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			if link, err = os.Readlink(path); err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = name
		if info.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		defer src.Close()
		n, err := io.CopyN(tw, src, info.Size())
		if err != nil || n != info.Size() {
			return fmt.Errorf("%s: %w", name, ErrChanged)
		}
		// Anything appended after the header was written means a torn copy.
		if extra, _ := io.CopyN(io.Discard, src, 1); extra > 0 {
			return fmt.Errorf("%s: %w", name, ErrChanged)
		}
		return nil
	})
}

// Prune removes this game's archives older than BACKUP_RETAIN_DAYS.
func (b *Backup) Prune() {
	if b.Cfg.BackupRetainDays <= 0 {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -b.Cfg.BackupRetainDays)
	entries, err := os.ReadDir(b.Cfg.BackupDir)
	if err != nil {
		return
	}
	removed := 0
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasPrefix(n, b.GameName+"-") || !strings.HasSuffix(n, ".tar.gz") {
			continue
		}
		if info, err := e.Info(); err == nil && info.ModTime().Before(cutoff) {
			if os.Remove(filepath.Join(b.Cfg.BackupDir, n)) == nil {
				removed++
			}
		}
	}
	if removed > 0 {
		logx.Infof("pruned %d archive(s) older than %d days", removed, b.Cfg.BackupRetainDays)
	}
}

// Human renders a byte count for logs and listings.
func Human(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
