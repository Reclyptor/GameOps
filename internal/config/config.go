// Package config reads and validates the common environment contract
// (docs/CONTRACT.md §7). Defaults live here and nowhere else.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/Reclyptor/GameOps/internal/cron"
)

// Config is the toolkit's view of the environment. Game-specific variables
// are the adapter's business and are passed through untouched.
type Config struct {
	Home    string // GAMEOPS_HOME
	State   string // GAMEOPS_STATE
	Adapter string // GAME_ADAPTER

	DataDir   string
	BackupDir string

	BackupEnabled       bool
	BackupCron          string
	BackupRetainDays    int
	BackupOnUpdate      bool
	BackupSettleSeconds int

	UpdateEnabled           bool
	UpdateCron              string
	UpdateOnBoot            bool
	UpdateWarnMinutes       int
	UpdateSkipIfPlayers     bool
	UpdateForceAfterMinutes int
	UpdateApplyTimeout      int // seconds; the whole game_update_apply, killed on expiry
	StallSeconds            int // seconds without output before a download is declared stuck

	ReadyTimeout        int
	StopTimeout         int
	PlayerEventsEnabled bool
	MetricsPort         int // 0 disables the listener
	GateEnabled         bool
	GatePort            int    // public port the gate listens on (the game's PORT)
	GateTargetPort      int    // where the game itself listens, loopback only
	GateExpect          string // marker the first bytes must contain; empty = any bytes
	GateTimeoutSeconds  int
	LogLevel            string

	NotifyProvider    string
	DiscordWebhookURL string
	DiscordSuppress   bool
	DiscordSilent     map[string]bool // events always sent with Discord's silent flag
	DiscordEmbeds     bool
	DiscordUsername   string
}

// IsTrue accepts the usual spellings of a boolean environment value.
func IsTrue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes", "on":
		return true
	}
	return false
}

func isBool(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "false", "1", "0", "yes", "no", "on", "off":
		return true
	}
	return false
}

func env(name, def string) string {
	if v, ok := os.LookupEnv(name); ok && v != "" {
		return v
	}
	return def
}

// Load reads the environment, applies defaults, and validates. Every
// problem is reported at once so an operator fixes a manifest in one pass.
func Load() (*Config, error) {
	c := &Config{
		Home:    env("GAMEOPS_HOME", "/opt/gameops"),
		State:   env("GAMEOPS_STATE", "/tmp/gameops"),
		Adapter: env("GAME_ADAPTER", "/opt/game/adapter.sh"),

		DataDir:   env("DATA_DIR", "/data"),
		BackupDir: env("BACKUP_DIR", "/backups"),

		BackupCron: env("BACKUP_CRON", "0 4 * * *"),
		UpdateCron: env("UPDATE_CRON", "0 * * * *"),
		LogLevel:   env("LOG_LEVEL", "info"),

		NotifyProvider:    env("NOTIFY_PROVIDER", "discord"),
		DiscordWebhookURL: env("DISCORD_WEBHOOK_URL", ""),
		DiscordUsername:   env("DISCORD_USERNAME", ""),
	}

	var errs []string
	boolVar := func(name, def string) bool {
		v := env(name, def)
		if !isBool(v) {
			errs = append(errs, fmt.Sprintf("%s must be a boolean, got %q", name, v))
		}
		return IsTrue(v)
	}
	intVar := func(name, def string) int {
		v := env(name, def)
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			errs = append(errs, fmt.Sprintf("%s must be a non-negative integer, got %q", name, v))
		}
		return n
	}

	c.BackupEnabled = boolVar("BACKUP_ENABLED", "true")
	c.BackupRetainDays = intVar("BACKUP_RETAIN_DAYS", "14")
	c.BackupOnUpdate = boolVar("BACKUP_ON_UPDATE", "true")
	c.BackupSettleSeconds = intVar("BACKUP_SETTLE_SECONDS", "10")
	c.UpdateEnabled = boolVar("UPDATE_ENABLED", "true")
	c.UpdateOnBoot = boolVar("UPDATE_ON_BOOT", "true")
	c.UpdateWarnMinutes = intVar("UPDATE_WARN_MINUTES", "15")
	c.UpdateSkipIfPlayers = boolVar("UPDATE_SKIP_IF_PLAYERS", "false")
	c.UpdateForceAfterMinutes = intVar("UPDATE_FORCE_AFTER_MINUTES", "30")
	c.UpdateApplyTimeout = intVar("UPDATE_APPLY_TIMEOUT", "3600")
	c.StallSeconds = intVar("STALL_SECONDS", "300")
	c.ReadyTimeout = intVar("READY_TIMEOUT", "900")
	c.StopTimeout = intVar("STOP_TIMEOUT", "120")
	c.PlayerEventsEnabled = boolVar("PLAYER_EVENTS_ENABLED", "true")
	c.MetricsPort = intVar("METRICS_PORT", "9110")
	c.GateEnabled = boolVar("GATE_ENABLED", "false")
	c.GatePort = intVar("GATE_PORT", env("PORT", "0"))
	c.GateTargetPort = intVar("GATE_TARGET_PORT", "0")
	c.GateExpect = env("GATE_EXPECT", "")
	c.GateTimeoutSeconds = intVar("GATE_TIMEOUT_SECONDS", "3")
	if c.GateEnabled {
		if c.GatePort == 0 || c.GateTargetPort == 0 {
			errs = append(errs, "GATE_ENABLED needs GATE_PORT (or PORT) and GATE_TARGET_PORT")
		} else if c.GatePort == c.GateTargetPort {
			errs = append(errs, "GATE_PORT and GATE_TARGET_PORT must differ")
		}
	}
	if c.MetricsPort > 65535 {
		errs = append(errs, fmt.Sprintf("METRICS_PORT must be 0 (off) or a port number, got %d", c.MetricsPort))
	}
	c.DiscordSuppress = boolVar("DISCORD_SUPPRESS_NOTIFICATIONS", "false")
	c.DiscordSilent = map[string]bool{}
	for _, ev := range strings.Split(env("DISCORD_SILENT_EVENTS", "BACKUP_PRE,BACKUP_POST"), ",") {
		if ev = strings.ToUpper(strings.TrimSpace(ev)); ev != "" {
			c.DiscordSilent[ev] = true
		}
	}
	c.DiscordEmbeds = boolVar("DISCORD_EMBEDS", "false")

	if _, ok := parseLevel(c.LogLevel); !ok {
		errs = append(errs, fmt.Sprintf("LOG_LEVEL must be debug|info|warn|error, got %q", c.LogLevel))
	}
	if c.BackupEnabled {
		if _, err := cron.Parse(c.BackupCron); err != nil {
			errs = append(errs, fmt.Sprintf("BACKUP_CRON is not a valid cron expression: %v", err))
		}
	}
	if c.UpdateEnabled {
		if _, err := cron.Parse(c.UpdateCron); err != nil {
			errs = append(errs, fmt.Sprintf("UPDATE_CRON is not a valid cron expression: %v", err))
		}
	}
	switch c.NotifyProvider {
	case "discord", "none":
	default:
		errs = append(errs, fmt.Sprintf("NOTIFY_PROVIDER must be discord|none, got %q", c.NotifyProvider))
	}

	if len(errs) > 0 {
		return nil, fmt.Errorf("invalid configuration:\n  %s", strings.Join(errs, "\n  "))
	}
	return c, nil
}

func parseLevel(s string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug", "info", "", "warn", "warning", "error":
		return s, true
	}
	return s, false
}

// RequireWritable refuses to run against a directory the current user cannot
// write. There is no chown path by design (CONTRACT.md §2.2).
func RequireWritable(path, what string) error {
	_ = os.MkdirAll(path, 0o755)
	st, err := os.Stat(path)
	if err != nil || !st.IsDir() {
		return fmt.Errorf("%s %q does not exist and could not be created", what, path)
	}
	f, err := os.CreateTemp(path, ".gameops-write-probe-*")
	if err != nil {
		return fmt.Errorf("%s %q is not writable by uid %d; fix the volume ownership (no root/chown path exists by design)", what, path, os.Getuid())
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return nil
}
