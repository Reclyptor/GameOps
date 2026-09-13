// Package notify sends lifecycle notifications (docs/CONTRACT.md §7). One
// provider today, Discord; the switch is here so a second provider is a new
// send function and nothing else.
package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/Reclyptor/GameOps/internal/config"
	"github.com/Reclyptor/GameOps/internal/logx"
)

var defaults = map[string]string{
	"START":           "🟢 server_name server is online — connect to `server_address` (password: `game_password`).",
	"STOP":            "💤 server_name server has shut down.",
	"CRASH":           "💥 server_name server stopped unexpectedly (reason).",
	"UPDATE_PRE":      "⏳ server_name server is updating to version — restart_note.",
	"UPDATE_POST":     "🚀 server_name server updated to version.",
	"UPDATE_DEFERRED": "⏸️ server_name server update to version deferred: players online.",
	"UPDATE_FAILED":   "❌ Update of the server_name server to version failed: reason — still running the installed version.",
	"BACKUP_PRE":      "💾 backup_kind backup of the server_name server…",
	"BACKUP_POST":     "✅ backup_kind backup of the server_name server complete: file_path",
	"BACKUP_FAILED":   "❌ backup_kind backup of the server_name server failed: reason",
	"RESTORE_PRE":     "♻️ server_name server is restoring file_path — restart_note.",
	"RESTORE_POST":    "♻️ server_name server restored from file_path.",
	"RESTORE_FAILED":  "❌ Restore of the server_name server from file_path failed: reason",
	"JOIN":            "🟢 **player_name** joined the server_name server.",
	"LEAVE":           "🔴 **player_name** left the server_name server.",
}

func level(event string) string {
	switch event {
	case "START", "UPDATE_POST", "BACKUP_POST", "RESTORE_POST":
		return "success"
	case "CRASH", "BACKUP_FAILED", "RESTORE_FAILED", "UPDATE_FAILED":
		return "failure"
	case "UPDATE_PRE", "BACKUP_PRE", "RESTORE_PRE":
		return "in-progress"
	case "UPDATE_DEFERRED":
		return "warn"
	}
	return "info"
}

func color(level string) int {
	switch level {
	case "success":
		return 52224
	case "failure":
		return 14614528
	case "in-progress":
		return 15258703
	case "warn":
		return 14177041
	}
	return 1127128
}

type Notifier struct {
	cfg        *config.Config
	serverName string
	client     *http.Client
	// OnFailure, when set, is called for every delivery that fails.
	OnFailure func()
}

func New(cfg *config.Config, serverName string) *Notifier {
	return &Notifier{cfg: cfg, serverName: serverName, client: &http.Client{Timeout: 20 * time.Second}}
}

// Render produces the message for an event: the per-event override or the
// default, with placeholder words replaced verbatim. Caller-supplied
// key=value pairs are applied first so a game-specific token can never be
// clobbered by the generic ones.
func (n *Notifier) Render(event string, kv []string) string {
	msg := os.Getenv("DISCORD_" + event + "_MESSAGE")
	if msg == "" {
		msg = defaults[event]
	}
	if msg == "" {
		msg = "server_name: reason"
	}
	tmpl := msg
	for _, pair := range kv {
		k, v, ok := strings.Cut(pair, "=")
		if ok && k != "" {
			msg = strings.ReplaceAll(msg, k, v)
		}
	}
	// Connection details, so a START message can tell players everything
	// they need: the operator-provided address and the join password.
	if addr := os.Getenv("SERVER_ADDRESS"); addr != "" {
		msg = strings.ReplaceAll(msg, "server_address", addr)
	} else {
		msg = dropAddressFragment(msg)
	}
	if pw := joinPassword(); pw != "" {
		msg = strings.ReplaceAll(msg, "game_password", pw)
	} else {
		msg = dropPasswordFragment(msg)
	}
	msg = strings.ReplaceAll(msg, "server_name", n.serverName)
	// A START message always tells players how to connect: whatever the
	// configured text says, the address and the password (when set) are
	// appended unless the text already places them itself.
	if event == "START" {
		var extra string
		if addr := os.Getenv("SERVER_ADDRESS"); addr != "" && !strings.Contains(tmpl, "server_address") {
			extra += " — connect to " + addr
		}
		if pw := joinPassword(); pw != "" && !strings.Contains(tmpl, "game_password") {
			extra += " (password: " + pw + ")"
		}
		if extra != "" {
			// Keep the sentence's own full stop at the end.
			if strings.HasSuffix(msg, ".") {
				msg = strings.TrimSuffix(msg, ".") + extra + "."
			} else {
				msg += extra
			}
		}
	}
	return msg
}

var inlineAddress = regexp.MustCompile("(?i)\\s*[,;:—–-]?\\s*(connect to|at|on)?\\s*[`'\"]*server_address[`'\"]*")

// dropAddressFragment removes the "— connect to server_address" clause (or
// the bare token) when no address is configured.
func dropAddressFragment(msg string) string {
	if !strings.Contains(msg, "server_address") {
		return msg
	}
	if out := inlineAddress.ReplaceAllString(msg, ""); out != msg {
		return out
	}
	return strings.ReplaceAll(msg, "server_address", "")
}

var (
	bracketedPassword = regexp.MustCompile("\\s*[(\\[{][^()\\[\\]{}]*game_password[^()\\[\\]{}]*[)\\]}]")
	inlinePassword    = regexp.MustCompile("(?i)\\s*[,;:—–-]?\\s*password\\s*[:=]?\\s*[`'\"]*game_password[`'\"]*")
)

// dropPasswordFragment removes whatever a message says about the password
// when there is none: a bracketed clause such as "(password: `game_password`)",
// an inline one such as ", password game_password", or, failing both, the bare
// token. An empty password must never render as "(password: )".
func dropPasswordFragment(msg string) string {
	if !strings.Contains(msg, "game_password") {
		return msg
	}
	if out := bracketedPassword.ReplaceAllString(msg, ""); out != msg {
		return out
	}
	if out := inlinePassword.ReplaceAllString(msg, ""); out != msg {
		return out
	}
	return strings.ReplaceAll(msg, "game_password", "")
}

// joinPassword is GAME_PASSWORD — the one name every adapter uses for the
// join password (CONTRACT.md §7) — or empty.
func joinPassword() string { return os.Getenv("GAME_PASSWORD") }

func (n *Notifier) Enabled(event string) bool {
	v, ok := os.LookupEnv("DISCORD_" + event + "_ENABLED")
	if !ok || v == "" {
		return true
	}
	return config.IsTrue(v)
}

// Send delivers one event. Failures are logged, never fatal.
func (n *Notifier) Send(event string, kv ...string) {
	if !n.Enabled(event) {
		return
	}
	msg := n.Render(event, kv)
	switch n.cfg.NotifyProvider {
	case "discord":
		if n.cfg.DiscordWebhookURL == "" {
			return
		}
		if err := n.discord(msg, level(event), n.cfg.DiscordSuppress || n.cfg.DiscordSilent[event]); err != nil {
			logx.Warnf("discord webhook delivery failed for: %s (%v)", msg, err)
			if n.OnFailure != nil {
				n.OnFailure()
			}
			return
		}
		logx.Debugf("notified discord: %s", msg)
	case "none":
	}
}

// Payload builds the Discord webhook body; silent sets Discord's
// SUPPRESS_NOTIFICATIONS flag so the post does not ping anyone.
func (n *Notifier) Payload(message, lvl string, silent bool) []byte {
	flags := 0
	if silent {
		flags = 4096
	}
	body := map[string]any{"flags": flags}
	if n.cfg.DiscordEmbeds {
		body["embeds"] = []map[string]any{{"description": message, "color": color(lvl)}}
	} else {
		body["content"] = message
	}
	if n.cfg.DiscordUsername != "" {
		body["username"] = n.cfg.DiscordUsername
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.Encode(body)
	return bytes.TrimRight(buf.Bytes(), "\n")
}

func (n *Notifier) discord(message, lvl string, silent bool) error {
	resp, err := n.client.Post(n.cfg.DiscordWebhookURL, "application/json", bytes.NewReader(n.Payload(message, lvl, silent)))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}
