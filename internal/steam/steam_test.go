package steam

import (
	"os"
	"path/filepath"
	"testing"
)

const acf = `"AppState"
{
	"appid"		"2394010"
	"name"		"Palworld Dedicated Server"
	"buildid"		"13378465"
	"InstalledDepots"
	{
		"1006"
		{
			"manifest"		"6403079453713498174"
			"size"		"1"
		}
		"2394012"
		{
			"manifest"		"9082869442509405085"
			"size"		"2"
		}
	}
}
`

func TestLocalManifest(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "steamapps"), 0o755)
	os.WriteFile(filepath.Join(dir, "steamapps", "appmanifest_2394010.acf"), []byte(acf), 0o644)
	if m, err := LocalManifest(dir, "2394010", "2394012"); err != nil || m != "9082869442509405085" {
		t.Fatalf("got %q, %v", m, err)
	}
	if m, err := LocalManifest(dir, "2394010", "1006"); err != nil || m != "6403079453713498174" {
		t.Fatalf("got %q, %v", m, err)
	}
	if _, err := LocalManifest(dir, "2394010", "999"); err == nil {
		t.Fatal("expected error for unknown depot")
	}
	if _, err := LocalManifest(dir, "1", "1"); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestParseVDFComments(t *testing.T) {
	tree, err := ParseVDF("// comment\n\"a\" { \"b\" \"c\" \"d\" { \"e\" \"f\" } }")
	if err != nil {
		t.Fatal(err)
	}
	a := tree["a"].(map[string]any)
	if a["b"] != "c" || a["d"].(map[string]any)["e"] != "f" {
		t.Fatalf("tree %v", tree)
	}
}
