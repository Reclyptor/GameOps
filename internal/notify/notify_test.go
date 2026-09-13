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

// A server whose clients cannot use the configured hostname (Core Keeper, whose
// join-by-IP field does not resolve names) reports the address players can
// actually use; that value is the truth and wins over SERVER_ADDRESS.
func TestReportedAddressIsAuthoritative(t *testing.T) {
	n := New(&config.Config{}, "CK")
	t.Setenv("SERVER_ADDRESS", "corekeeper.example.io:10530")
	t.Setenv("GAME_PASSWORD", "makoto")

	if got := n.Render("START", []string{"server_address=69.9.179.16:10530"}); got != "🟢 CK server is online — connect to `69.9.179.16:10530` (password: `makoto`)." {
		t.Fatalf("reported address not announced: %q", got)
	}
	// Reported empty: no address fragment, whatever SERVER_ADDRESS says.
	if got := n.Render("START", []string{"server_address="}); got != "🟢 CK server is online (password: `makoto`)." {
		t.Fatalf("an empty reported address must render no fragment: %q", got)
	}
	// Both reported at once.
	if got := n.Render("START", []string{"server_address=69.9.179.16:10530", "game_password=flumpy"}); got != "🟢 CK server is online — connect to `69.9.179.16:10530` (password: `flumpy`)." {
		t.Fatalf("both reported values must be used: %q", got)
	}
	// A custom text without the placeholder still gets the reported address.
	t.Setenv("DISCORD_START_MESSAGE", "server_name is up.")
	if got := n.Render("START", []string{"server_address=69.9.179.16:10530"}); got != "CK is up — connect to 69.9.179.16:10530 (password: makoto)." {
		t.Fatalf("reported address not appended to a custom text: %q", got)
	}
	// Not reported: SERVER_ADDRESS, as before.
	t.Setenv("DISCORD_START_MESSAGE", "")
	if got := n.Render("START", nil); got != "🟢 CK server is online — connect to `corekeeper.example.io:10530` (password: `makoto`)." {
		t.Fatalf("SERVER_ADDRESS fallback broken: %q", got)
	}
}

// A server that makes up its own password (Core Keeper, when GAME_PASSWORD is
// empty or invalid) reports it through game_password=…; that value is the
// truth and wins over the environment in every direction.
func TestReportedPasswordIsAuthoritative(t *testing.T) {
	n := New(&config.Config{}, "CK")
	t.Setenv("SERVER_ADDRESS", "ck.example.io:10530")

	// Nothing configured, the server generated one: it is announced.
	t.Setenv("GAME_PASSWORD", "")
	if got := n.Render("START", []string{"game_password=Gen3rated"}); got != "🟢 CK server is online — connect to `ck.example.io:10530` (password: `Gen3rated`)." {
		t.Fatalf("generated password not announced: %q", got)
	}
	// Configured but replaced by the server: the one in force is announced.
	t.Setenv("GAME_PASSWORD", "way-too-long-for-the-game-to-accept-it")
	if got := n.Render("START", []string{"game_password=Repl4ced"}); got != "🟢 CK server is online — connect to `ck.example.io:10530` (password: `Repl4ced`)." {
		t.Fatalf("configured password must not override the reported one: %q", got)
	}
	// Reported empty (e.g. a mode that takes no password): no fragment, even
	// though GAME_PASSWORD is set.
	if got := n.Render("START", []string{"game_password="}); got != "🟢 CK server is online — connect to `ck.example.io:10530`." {
		t.Fatalf("an empty reported password must render no fragment: %q", got)
	}
	// A custom text without the placeholder still gets the reported password.
	t.Setenv("DISCORD_START_MESSAGE", "server_name is up.")
	if got := n.Render("START", []string{"game_password=Gen3rated"}); got != "CK is up — connect to ck.example.io:10530 (password: Gen3rated)." {
		t.Fatalf("reported password not appended to a custom text: %q", got)
	}
	// Not reported at all: GAME_PASSWORD, as before.
	t.Setenv("DISCORD_START_MESSAGE", "")
	t.Setenv("GAME_PASSWORD", "from-env")
	if got := n.Render("START", nil); got != "🟢 CK server is online — connect to `ck.example.io:10530` (password: `from-env`)." {
		t.Fatalf("GAME_PASSWORD fallback broken: %q", got)
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

// ALL must cover events nobody listed — including ones an adapter invents —
// so a new event type can never arrive loud by accident.
func TestSilent(t *testing.T) {
	quiet := func(events, event string) bool {
		t.Setenv("DISCORD_SILENT_EVENTS", events)
		t.Setenv("DISCORD_SUPPRESS_NOTIFICATIONS", "false")
		cfg, err := config.Load()
		if err != nil {
			t.Fatal(err)
		}
		return New(cfg, "S").Silent(event)
	}
	for _, tc := range []struct {
		events, event string
		want          bool
	}{
		{"BACKUP_PRE,BACKUP_POST", "BACKUP_PRE", true},
		{"BACKUP_PRE,BACKUP_POST", "START", false},
		{"ALL,!JOIN,!LEAVE", "START", true},
		{"ALL,!JOIN,!LEAVE", "BACKUP_POST", true},
		{"ALL,!JOIN,!LEAVE", "GAMEID", true}, // an adapter's own event
		{"ALL,!JOIN,!LEAVE", "JOIN", false},
		{"ALL,!JOIN,!LEAVE", "LEAVE", false},
		{"ALL", "JOIN", true},
		{"", "START", false},
	} {
		if got := quiet(tc.events, tc.event); got != tc.want {
			t.Errorf("DISCORD_SILENT_EVENTS=%q: Silent(%s) = %v, want %v", tc.events, tc.event, got, tc.want)
		}
	}

	// The global switch still silences everything, exemptions included.
	t.Setenv("DISCORD_SILENT_EVENTS", "ALL,!JOIN")
	t.Setenv("DISCORD_SUPPRESS_NOTIFICATIONS", "true")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !New(cfg, "S").Silent("JOIN") {
		t.Error("DISCORD_SUPPRESS_NOTIFICATIONS must silence even an exempted event")
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

func TestEnabledPolicy(t *testing.T) {
	t.Setenv("DISCORD_DISABLED_EVENTS", "ALL,!JOIN,!LEAVE")
	c, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	n := New(c, "Test Server")

	for _, ev := range []string{"JOIN", "LEAVE"} {
		if !n.Enabled(ev) {
			t.Fatalf("%s was exempted and must still be sent", ev)
		}
	}
	for _, ev := range []string{"START", "STOP", "BACKUP_POST", "CRASH"} {
		if n.Enabled(ev) {
			t.Fatalf("%s should be omitted, not sent", ev)
		}
	}
	// The reason this is a policy and not a list: an event no one has heard of
	// yet must not arrive in a channel that asked for quiet.
	if n.Enabled("GAMEID") {
		t.Fatal("an event type invented by an adapter must be covered by ALL")
	}
	// A switch aimed at one event beats the blanket policy.
	t.Setenv("DISCORD_CRASH_ENABLED", "true")
	c2, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !New(c2, "Test Server").Enabled("CRASH") {
		t.Fatal("DISCORD_CRASH_ENABLED=true should win over ALL")
	}
}
