package config

import (
	"strings"
	"testing"
)

func TestDefaults(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.BackupCron != "0 4 * * *" || c.BackupRetainDays != 14 || !c.UpdateOnBoot || c.StopTimeout != 120 || c.ReadyTimeout != 900 {
		t.Fatalf("defaults wrong: %+v", c)
	}
}

func TestRejects(t *testing.T) {
	t.Setenv("UPDATE_ON_BOOT", "maybe")
	t.Setenv("STOP_TIMEOUT", "soon")
	t.Setenv("BACKUP_CRON", "0 4 * *")
	t.Setenv("LOG_LEVEL", "chatty")
	_, err := Load()
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"UPDATE_ON_BOOT must be a boolean", "STOP_TIMEOUT must be a non-negative integer", "LOG_LEVEL must be", "BACKUP_CRON is not a valid cron expression"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}

func TestDisabledJobCronNotValidated(t *testing.T) {
	t.Setenv("BACKUP_ENABLED", "false")
	t.Setenv("BACKUP_CRON", "garbage")
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
}

func TestIsTrue(t *testing.T) {
	for _, v := range []string{"true", "TRUE", "1", "yes", "on"} {
		if !IsTrue(v) {
			t.Errorf("%q should be true", v)
		}
	}
	for _, v := range []string{"false", "0", "no", "", "maybe"} {
		if IsTrue(v) {
			t.Errorf("%q should be false", v)
		}
	}
}

func TestRequireWritable(t *testing.T) {
	dir := t.TempDir()
	if err := RequireWritable(dir+"/new", "DATA_DIR"); err != nil {
		t.Fatal(err)
	}
	if err := RequireWritable("/proc/nope", "DATA_DIR"); err == nil {
		t.Fatal("expected error")
	}
}
