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

func TestLockTimeout(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	// 30 min countdown + 120 stop + 3600 apply + 900 ready + 3600 backups.
	if c.LockTimeout != 10020 {
		t.Fatalf("default LOCK_TIMEOUT = %d, want 10020", c.LockTimeout)
	}

	// The default follows the limits it is built from.
	t.Setenv("UPDATE_APPLY_TIMEOUT", "7200")
	t.Setenv("UPDATE_WARN_MINUTES", "45")
	if c, err = Load(); err != nil {
		t.Fatal(err)
	}
	if want := 45*60 + 120 + 7200 + 900 + 3600; c.LockTimeout != want {
		t.Fatalf("derived LOCK_TIMEOUT = %d, want %d", c.LockTimeout, want)
	}

	t.Setenv("LOCK_TIMEOUT", "0")
	if c, err = Load(); err != nil || c.LockTimeout != 0 {
		t.Fatalf("LOCK_TIMEOUT=0 (try once) must be accepted: %v %+v", err, c)
	}
	t.Setenv("LOCK_TIMEOUT", "90")
	if c, err = Load(); err != nil || c.LockTimeout != 90 {
		t.Fatalf("explicit LOCK_TIMEOUT must win: %v", err)
	}
	t.Setenv("LOCK_TIMEOUT", "forever")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "LOCK_TIMEOUT must be a non-negative integer") {
		t.Fatalf("expected LOCK_TIMEOUT rejection, got %v", err)
	}
}

func TestSilentEvents(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.DiscordSilent["BACKUP_PRE"] || !c.DiscordSilent["BACKUP_POST"] || c.DiscordSilentAll {
		t.Fatalf("default should silence only the backup posts: %+v", c.DiscordSilent)
	}

	t.Setenv("DISCORD_SILENT_EVENTS", "all, !join , !LEAVE")
	if c, err = Load(); err != nil {
		t.Fatal(err)
	}
	if !c.DiscordSilentAll || !c.DiscordLoud["JOIN"] || !c.DiscordLoud["LEAVE"] || len(c.DiscordSilent) != 0 {
		t.Fatalf("ALL with exemptions parsed wrong: %+v", c)
	}

	t.Setenv("DISCORD_SILENT_EVENTS", "ALL,!JOIN,JOIN")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "both silent and loud") {
		t.Fatalf("an event listed both ways must be rejected, got %v", err)
	}

	t.Setenv("DISCORD_SILENT_EVENTS", "ALL,!")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "no event after it") {
		t.Fatalf("a bare ! must be rejected, got %v", err)
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

func TestDisabledEventsPolicy(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.DiscordOffAll || len(c.DiscordOff) != 0 {
		t.Fatalf("nothing should be disabled by default: %+v", c.DiscordOff)
	}

	t.Setenv("DISCORD_DISABLED_EVENTS", "all, !join , !LEAVE")
	c, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.DiscordOffAll || !c.DiscordOn["JOIN"] || !c.DiscordOn["LEAVE"] || len(c.DiscordOff) != 0 {
		t.Fatalf("ALL,!JOIN,!LEAVE should disable everything but joins and leaves: %+v", c)
	}

	// The two lists are independent: silencing and omitting are different acts.
	t.Setenv("DISCORD_SILENT_EVENTS", "BACKUP_PRE")
	c, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.DiscordSilent["BACKUP_PRE"] || c.DiscordOff["BACKUP_PRE"] {
		t.Fatalf("a silenced event must not become a disabled one: %+v", c)
	}

	t.Setenv("DISCORD_DISABLED_EVENTS", "ALL,!JOIN,JOIN")
	if _, err = Load(); err == nil {
		t.Fatal("listing an event both disabled and enabled should be an error")
	} else if !strings.Contains(err.Error(), "DISCORD_DISABLED_EVENTS") {
		t.Fatalf("the error should name the variable the operator set: %v", err)
	}

	t.Setenv("DISCORD_DISABLED_EVENTS", "ALL,!")
	if _, err = Load(); err == nil {
		t.Fatal("a bare ! should be an error")
	}
}

func TestRequiredGraceTracksStopTimeout(t *testing.T) {
	// The grace period must always outlast the drain plus the graceful stop, or
	// the orchestrator SIGKILLs the container mid-save.
	c := &Config{StopTimeout: 120}
	if got, want := c.RequiredGrace(300), 300+120+graceMargin; got != want {
		t.Errorf("RequiredGrace(300) = %d, want %d", got, want)
	}
	// Raising STOP_TIMEOUT must raise the requirement, not be silently ignored.
	slower := &Config{StopTimeout: 600}
	if slower.RequiredGrace(300) <= c.RequiredGrace(300) {
		t.Error("a longer STOP_TIMEOUT must require a longer grace period")
	}
	if got := c.RequiredGrace(0); got != 120+graceMargin {
		t.Errorf("RequiredGrace(0) = %d, want %d", got, 120+graceMargin)
	}
}
