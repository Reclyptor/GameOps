package steam

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Reclyptor/GameOps/internal/logx"
)

// Update brings dir to the current build. It tries in place first; when
// SteamCMD keeps failing against the existing installation (a stale
// appmanifest, a half-finished download — the usual "Missing configuration"
// and "state is 0x6" endings), it installs the build fresh into a staging
// directory next to the live one and swaps the two by rename, carrying the
// keep paths (the game's own data, e.g. Pal/Saved) across. A failed swap is
// rolled back, so the live installation is never left half-replaced.
func Update(steamcmdDir, dir string, appIDs []string, stall time.Duration, keep []string) error {
	if err := Install(steamcmdDir, dir, appIDs, stall); err == nil {
		return nil
	} else {
		logx.Warnf("in-place update failed (%v); installing fresh and swapping", err)
	}
	staging := filepath.Join(dir, ".steam-staging")
	os.RemoveAll(staging)
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return err
	}
	if err := Install(steamcmdDir, staging, appIDs, stall); err != nil {
		os.RemoveAll(staging)
		return fmt.Errorf("fresh install failed too: %w", err)
	}
	if err := swapInstall(dir, staging, keep); err != nil {
		return err
	}
	logx.Infof("installation replaced from a fresh download")
	return nil
}

// swapInstall replaces dir's top-level entries with staging's, then moves the
// keep paths from the previous tree back into place. Everything is a rename
// on one filesystem; on any failure the renames done so far are undone.
func swapInstall(dir, staging string, keep []string) error {
	old := filepath.Join(dir, ".steam-old")
	os.RemoveAll(old)
	if err := os.MkdirAll(old, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(staging)
	if err != nil {
		return err
	}
	var swapped []string
	rollback := func(cause error) error {
		for _, name := range swapped {
			live := filepath.Join(dir, name)
			os.RemoveAll(live)
			if prev := filepath.Join(old, name); exists(prev) {
				os.Rename(prev, live)
			}
		}
		os.RemoveAll(staging)
		os.RemoveAll(old)
		return fmt.Errorf("swap aborted, installation unchanged: %w", cause)
	}
	// Live entries that the fresh install does not have (the keep paths'
	// parents included) stay where they are; only replaced entries move.
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".steam-") {
			continue
		}
		live := filepath.Join(dir, name)
		if exists(live) {
			if err := os.Rename(live, filepath.Join(old, name)); err != nil {
				return rollback(err)
			}
		}
		swapped = append(swapped, name)
		if err := os.Rename(filepath.Join(staging, name), live); err != nil {
			return rollback(err)
		}
	}
	for _, k := range keep {
		k = filepath.Clean(k)
		src := filepath.Join(old, k)
		if !exists(src) {
			continue
		}
		dst := filepath.Join(dir, k)
		os.RemoveAll(dst)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return rollback(err)
		}
		if err := os.Rename(src, dst); err != nil {
			return rollback(err)
		}
	}
	os.RemoveAll(old)
	os.RemoveAll(staging)
	return nil
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}
