// Package restore is `gameops restore` (docs/CONTRACT.md §4.5): put a
// verified archive back in place of the live data. With a server running the
// swap happens in the runner after a graceful stop, exactly like an update;
// without one it happens here and now.
package restore

import (
	"fmt"
	"strconv"

	"github.com/Reclyptor/GameOps/internal/adapter"
	"github.com/Reclyptor/GameOps/internal/backup"
	"github.com/Reclyptor/GameOps/internal/config"
	"github.com/Reclyptor/GameOps/internal/logx"
	"github.com/Reclyptor/GameOps/internal/notify"
	"github.com/Reclyptor/GameOps/internal/state"
	"github.com/Reclyptor/GameOps/internal/update"
)

type Restorer struct {
	Cfg *config.Config
	St  *state.Store
	Ad  *adapter.Adapter
	Nt  *notify.Notifier
	Bk  *backup.Backup
	Up  *update.Updater
}

// Run restores spec ("latest", an archive name or a path). safetyBackup
// takes one more backup of the live data first, so a restore is never a
// one-way door. Callers hold the shared lock.
func (r *Restorer) Run(spec string, safetyBackup bool) error {
	archive, err := r.Bk.Resolve(spec)
	if err != nil {
		return err
	}
	rep, err := backup.Verify(archive, nil)
	if err != nil {
		return fmt.Errorf("refusing to restore %s: %w", archive, err)
	}
	logx.Actionf("restore: %s (%d files, %s)", archive, rep.Entries, backup.Human(rep.Bytes))

	if r.St.ServerPID() == 0 {
		if _, err := backup.Restore(r.Cfg.DataDir, archive); err != nil {
			r.Nt.Send("RESTORE_FAILED", "file_path="+archive, "reason="+err.Error())
			return err
		}
		logx.Infof("restored from %s", archive)
		r.St.Bump(state.RestoresTotal)
		r.Nt.Send("RESTORE_POST", "file_path="+archive)
		return nil
	}

	r.Nt.Send("RESTORE_PRE", "file_path="+archive, "warn_minutes="+strconv.Itoa(r.Cfg.UpdateWarnMinutes), "restart_note="+r.Up.RestartNote())
	r.Up.Countdown("to restore a backup")
	if safetyBackup {
		if err := r.Bk.Run(backup.KindPreRestore); err != nil {
			return fmt.Errorf("safety backup failed, restore aborted (pass --no-backup to skip it): %w", err)
		}
	}
	logx.Infof("requesting server stop to restore %s", archive)
	return r.Up.StopForRelaunch("restore.requested", archive)
}
