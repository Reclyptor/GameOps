package jsonx

import (
	"strings"
	"testing"
)

const sample = `{"stable":{"headless":"2.0.77"},"data":{"1963720":{"depots":{"1963722":{"manifests":{"public":{"gid":"9082869442509405085"}}}}}},"mods":[{"name":"base","enabled":true},{"name":"quality","enabled":false}],"n":3,"nested":{"arr":[1,2,3]}}`

func TestGet(t *testing.T) {
	cases := map[string]string{
		".stable.headless": "2.0.77",
		`.data["1963720"].depots["1963722"].manifests.public.gid`: "9082869442509405085",
		".mods[1].name":    "quality",
		".mods[1].enabled": "false",
		".n":               "3",
		".nested.arr":      "[1,2,3]",
		".stable":          `{"headless":"2.0.77"}`,
		".mods.[0].name":   "base",
		".nested.arr.[2]":  "3",
		".stable.":         `{"headless":"2.0.77"}`,
	}
	for path, want := range cases {
		got, err := Get([]byte(sample), path)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		if got != want {
			t.Errorf("%s: got %q want %q", path, got, want)
		}
	}
	if _, err := Get([]byte(sample), ".missing.key"); err != ErrMissing {
		t.Errorf("expected ErrMissing, got %v", err)
	}
	if _, err := Get([]byte("not json"), ".a"); err == nil {
		t.Error("expected parse error")
	}
	for _, path := range []string{"..a", "a", ".mods[x]", ".mods[0"} {
		if _, err := Get([]byte(sample), path); err == nil || err == ErrMissing {
			t.Errorf("%s: expected a path error, got %v", path, err)
		}
	}
	if got, err := Get([]byte(`["x","y"]`), ".[1]"); err != nil || got != "y" {
		t.Errorf(".[1] on an array: got %q, %v", got, err)
	}
}

func TestSet(t *testing.T) {
	out, err := Set([]byte(`{"name":"x","visibility":{"public":false}}`), ".name", "Ada's \"Factory\"", false)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := Get(out, ".name"); v != `Ada's "Factory"` {
		t.Fatalf("got %q", v)
	}
	out, err = Set(out, ".visibility.lan", "true", true)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := Get(out, ".visibility.lan"); v != "true" {
		t.Fatalf("got %q", v)
	}
	out, err = Set(out, ".tags", `["a","b"]`, true)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := Get(out, ".tags[1]"); v != "b" {
		t.Fatalf("got %q", v)
	}
	if _, err := Set(out, ".n", "not json", true); err == nil {
		t.Error("expected error for raw non-JSON")
	}
	out, _ = Set(nil, ".a.b", "c", false)
	if v, _ := Get(out, ".a.b"); v != "c" {
		t.Fatalf("got %q", v)
	}
	if !strings.HasSuffix(string(out), "\n") {
		t.Error("expected trailing newline")
	}
}

func TestEscapeArray(t *testing.T) {
	if got := Escape(`he said "hi" \ <b>`); got != `"he said \"hi\" \\ <b>"` {
		t.Fatalf("got %s", got)
	}
	if got := Array([]string{"a", "b c"}); got != `["a","b c"]` {
		t.Fatalf("got %s", got)
	}
	if got := Array(nil); got != `[]` {
		t.Fatalf("got %s", got)
	}
}
