package notify

import (
	"encoding/json"
	"testing"

	"github.com/Reclyptor/GameOps/internal/config"
)

func TestRender(t *testing.T) {
	n := New(&config.Config{}, "Test Server")
	t.Setenv("SERVER_ADDRESS", "")
	t.Setenv("GAME_PASSWORD", "")
	if got := n.Render("START", nil); got != "🟢 Test Server server is online." {
		t.Fatalf("got %q", got)
	}
	t.Setenv("SERVER_ADDRESS", "play.example.net:34197")
	if got := n.Render("START", nil); got != "🟢 Test Server server is online — connect to `play.example.net:34197`." {
		t.Fatalf("got %q", got)
	}
	t.Setenv("GAME_PASSWORD", "example-pass")
	if got := n.Render("START", nil); got != "🟢 Test Server server is online — connect to `play.example.net:34197` (password: `example-pass`)." {
		t.Fatalf("got %q", got)
	}
	if got := n.Render("BACKUP_PRE", []string{"backup_kind=Pre-update"}); got != "💾 Pre-update backup of the Test Server server…" {
		t.Fatalf("got %q", got)
	}
	if got := n.Render("BACKUP_POST", []string{"backup_kind=Scheduled", "file_path=/backups/x.tar.gz"}); got != "✅ Scheduled backup of the Test Server server complete: /backups/x.tar.gz" {
		t.Fatalf("got %q", got)
	}
	if got := n.Render("JOIN", []string{"player_name=Alice"}); got != "🟢 **Alice** joined the Test Server server." {
		t.Fatalf("got %q", got)
	}
	if got := n.Render("UPDATE_PRE", []string{"version=2.0", "warn_minutes=15", "restart_note=restarting in 15 minutes"}); got != "⏳ Test Server server is updating to 2.0 — restarting in 15 minutes." {
		t.Fatalf("got %q", got)
	}
	t.Setenv("DISCORD_JOIN_MESSAGE", "player_name is here (game_id)")
	if got := n.Render("JOIN", []string{"player_name=Bob", "game_id=XYZ"}); got != "Bob is here (XYZ)" {
		t.Fatalf("got %q", got)
	}
	t.Setenv("DISCORD_GAMEID_MESSAGE", "id: game_id")
	if got := n.Render("GAMEID", []string{"game_id=abc"}); got != "id: abc" {
		t.Fatalf("got %q", got)
	}
}

func TestStartAlwaysCarriesConnectionDetails(t *testing.T) {
	n := New(&config.Config{}, "Factory")
	t.Setenv("SERVER_ADDRESS", "play.example.net:34197")
	t.Setenv("GAME_PASSWORD", "example-pass")
	// a custom text that mentions neither → both appended, full stop kept last
	t.Setenv("DISCORD_START_MESSAGE", "server_name is online.")
	if got := n.Render("START", nil); got != "Factory is online — connect to play.example.net:34197 (password: example-pass)." {
		t.Fatalf("got %q", got)
	}
	// custom text that places the address but not the password → password appended
	t.Setenv("DISCORD_START_MESSAGE", "Up at server_address!")
	if got := n.Render("START", nil); got != "Up at play.example.net:34197! (password: example-pass)" {
		t.Fatalf("got %q", got)
	}
	// no password configured → nothing about a password
	t.Setenv("GAME_PASSWORD", "")
	if got := n.Render("START", nil); got != "Up at play.example.net:34197!" {
		t.Fatalf("got %q", got)
	}
	// other events are left alone
	t.Setenv("GAME_PASSWORD", "example-pass")
	if got := n.Render("STOP", nil); got != "💤 Factory server has shut down." {
		t.Fatalf("got %q", got)
	}
}

func TestEmptyPasswordNeverRendersAFragment(t *testing.T) {
	n := New(&config.Config{}, "CK")
	t.Setenv("SERVER_ADDRESS", "ck.example.io:42432")
	t.Setenv("GAME_PASSWORD", "")
	cases := map[string]string{
		"⛏️ CK is online — direct connect to `server_address` (password: `game_password`).": "⛏️ CK is online — direct connect to `ck.example.io:42432`.",
		"Online at server_address [password game_password]":                                 "Online at ck.example.io:42432",
		"Online at server_address, password: game_password":                                 "Online at ck.example.io:42432",
		"Online at server_address — password game_password":                                 "Online at ck.example.io:42432",
		"Online at server_address game_password":                                            "Online at ck.example.io:42432 ",
		"Online at server_address":                                                          "Online at ck.example.io:42432",
	}
	for tmpl, want := range cases {
		t.Setenv("DISCORD_START_MESSAGE", tmpl)
		if got := n.Render("START", nil); got != want {
			t.Errorf("%q → got %q, want %q", tmpl, got, want)
		}
	}
	// with a password set the same templates render it
	t.Setenv("GAME_PASSWORD", "example-pass")
	t.Setenv("DISCORD_START_MESSAGE", "Online at server_address (password: `game_password`)")
	if got := n.Render("START", nil); got != "Online at ck.example.io:42432 (password: `example-pass`)" {
		t.Fatalf("got %q", got)
	}
}

func TestConnectionPlaceholders(t *testing.T) {
	n := New(&config.Config{}, "Factory")
	t.Setenv("DISCORD_START_MESSAGE", "server_name is online — connect to server_address, password game_password")
	t.Setenv("SERVER_ADDRESS", "play.example.net:34197")
	t.Setenv("GAME_PASSWORD", "example-pass")
	if got := n.Render("START", nil); got != "Factory is online — connect to play.example.net:34197, password example-pass" {
		t.Fatalf("got %q", got)
	}
	// Only GAME_PASSWORD is the join password; a game's own differently named
	// variable is never consulted.
	t.Setenv("GAME_PASSWORD", "")
	t.Setenv("SERVER_PASSWORD", "palpass")
	if got := n.Render("START", nil); got != "Factory is online — connect to play.example.net:34197" {
		t.Fatalf("got %q", got)
	}
}

func TestEnabled(t *testing.T) {
	n := New(&config.Config{}, "x")
	if !n.Enabled("LEAVE") {
		t.Fatal("default should be enabled")
	}
	t.Setenv("DISCORD_LEAVE_ENABLED", "false")
	if n.Enabled("LEAVE") {
		t.Fatal("should be disabled")
	}
}

func TestPayload(t *testing.T) {
	n := New(&config.Config{}, "x")
	var body map[string]any
	json.Unmarshal(n.Payload(`He said "hi" \ <@1> & done`, "info", false), &body)
	if body["content"] != `He said "hi" \ <@1> & done` || body["flags"].(float64) != 0 {
		t.Fatalf("plain payload wrong: %v", body)
	}
	if _, ok := body["embeds"]; ok {
		t.Fatal("no embeds expected")
	}
	n = New(&config.Config{DiscordSuppress: true, DiscordEmbeds: true, DiscordUsername: "Ops"}, "x")
	json.Unmarshal(n.Payload("boom", "failure", true), &body)
	if body["flags"].(float64) != 4096 || body["username"] != "Ops" {
		t.Fatalf("embed payload wrong: %v", body)
	}
	emb := body["embeds"].([]any)[0].(map[string]any)
	if emb["description"] != "boom" || emb["color"].(float64) != 14614528 {
		t.Fatalf("embed wrong: %v", emb)
	}
}
