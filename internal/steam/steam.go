// Package steam installs and updates Steam-distributed servers through
// SteamCMD (anonymous login) and detects updates by comparing the local app
// manifest with the public depot manifest.
package steam

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Reclyptor/GameOps/internal/httpx"
	"github.com/Reclyptor/GameOps/internal/jsonx"
	"github.com/Reclyptor/GameOps/internal/logx"
)

var InfoAPI = "https://api.steamcmd.net/v1/info"

// Install installs or updates every app into dir, validating files.
//
// SteamCMD keeps an app-info cache that goes stale once a new build is
// published; an update attempted against it fails with "Missing
// configuration" (exit 8) and keeps failing on plain retries. So every run
// asks for fresh app info first, and a failed attempt drops the cache before
// the next one.
//
// SteamCMD also hangs: a download that stops making progress never ends on
// its own. Output is watched, and a run that prints nothing for stall is
// killed and counted as a failed attempt.
func Install(steamcmdDir, dir string, appIDs []string, stall time.Duration) error {
	args := []string{"+@sSteamCmdForcePlatformType", "linux", "+@sSteamCmdForcePlatformBitness", "64",
		"+force_install_dir", dir, "+login", "anonymous", "+app_info_update", "1"}
	for _, id := range appIDs {
		args = append(args, "+app_update", id, "validate")
	}
	args = append(args, "+quit")
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		if err = runWatched(filepath.Join(steamcmdDir, "steamcmd.sh"), args, stall); err == nil {
			return nil
		}
		logx.Warnf("steamcmd attempt %d failed: %v; refreshing the app-info cache", attempt, err)
		dropAppInfoCache(steamcmdDir)
		time.Sleep(5 * time.Second)
	}
	return err
}

// ErrStalled reports a run killed for producing no output within the window.
var ErrStalled = errors.New("no output for the stall window")

// runWatched runs a command, mirroring its output, and kills its process
// group when the output goes quiet for longer than stall (0 = never).
func runWatched(bin string, args []string, stall time.Duration) error {
	cmd := exec.Command(bin, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		pw.Close()
		pr.Close()
		return err
	}
	pw.Close()
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	copied := make(chan struct{})
	go func() {
		buf := make([]byte, 4096)
		for {
			n, rerr := pr.Read(buf)
			if n > 0 {
				os.Stdout.Write(buf[:n])
				last.Store(time.Now().UnixNano())
			}
			if rerr != nil {
				break
			}
		}
		pr.Close()
		close(copied)
	}()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			<-copied
			return err
		case <-ticker.C:
			if stall > 0 && time.Since(time.Unix(0, last.Load())) > stall {
				syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				<-done
				<-copied
				return fmt.Errorf("%w (%s)", ErrStalled, stall)
			}
		}
	}
}

// dropAppInfoCache removes SteamCMD's cached app info so the next run fetches
// the current build's configuration.
func dropAppInfoCache(steamcmdDir string) {
	for _, p := range []string{
		filepath.Join(steamcmdDir, "appcache", "appinfo.vdf"),
		filepath.Join(os.Getenv("HOME"), "Steam", "appcache", "appinfo.vdf"),
	} {
		if err := os.Remove(p); err == nil {
			logx.Debugf("removed %s", p)
		}
	}
}

// LocalManifest reads the manifest id SteamCMD recorded for one depot.
func LocalManifest(dir, appID, depot string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "steamapps", "appmanifest_"+appID+".acf"))
	if err != nil {
		return "", err
	}
	tree, err := ParseVDF(string(data))
	if err != nil {
		return "", err
	}
	for _, root := range tree {
		app, _ := root.(map[string]any)
		depots, _ := app["InstalledDepots"].(map[string]any)
		d, _ := depots[depot].(map[string]any)
		if m, ok := d["manifest"].(string); ok && m != "" {
			return m, nil
		}
	}
	return "", fmt.Errorf("depot %s not found in appmanifest_%s.acf", depot, appID)
}

// RemoteManifest is the current public manifest id from the info API.
func RemoteManifest(appID, depot string) (string, error) {
	data, err := httpx.Get(InfoAPI + "/" + appID)
	if err != nil {
		return "", err
	}
	base := fmt.Sprintf(`.data["%s"].depots["%s"].manifests.public`, appID, depot)
	if gid, err := jsonx.Get(data, base+".gid"); err == nil && gid != "" {
		return gid, nil
	}
	if m, err := jsonx.Get(data, base); err == nil && m != "" && !strings.HasPrefix(m, "{") {
		return m, nil
	}
	return "", fmt.Errorf("no public manifest for app %s depot %s", appID, depot)
}

// UpdateAvailable compares local and remote manifests.
func UpdateAvailable(dir, appID, depot string) (remote string, available bool, err error) {
	local, err := LocalManifest(dir, appID, depot)
	if err != nil {
		return "", false, err
	}
	remote, err = RemoteManifest(appID, depot)
	if err != nil {
		return "", false, err
	}
	return remote, local != remote, nil
}

// ParseVDF parses the text KeyValues format Steam uses for .acf files into
// nested maps of strings.
func ParseVDF(src string) (map[string]any, error) {
	p := &vdfParser{src: src}
	root := map[string]any{}
	for {
		p.skipSpace()
		if p.pos >= len(p.src) {
			return root, nil
		}
		key, err := p.token()
		if err != nil {
			return nil, err
		}
		val, err := p.value()
		if err != nil {
			return nil, err
		}
		root[key] = val
	}
}

type vdfParser struct {
	src string
	pos int
}

func (p *vdfParser) skipSpace() {
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			p.pos++
			continue
		}
		if c == '/' && p.pos+1 < len(p.src) && p.src[p.pos+1] == '/' {
			for p.pos < len(p.src) && p.src[p.pos] != '\n' {
				p.pos++
			}
			continue
		}
		return
	}
}

func (p *vdfParser) token() (string, error) {
	p.skipSpace()
	if p.pos >= len(p.src) {
		return "", fmt.Errorf("unexpected end of input")
	}
	if p.src[p.pos] == '"' {
		p.pos++
		var sb strings.Builder
		for p.pos < len(p.src) && p.src[p.pos] != '"' {
			if p.src[p.pos] == '\\' && p.pos+1 < len(p.src) {
				p.pos++
			}
			sb.WriteByte(p.src[p.pos])
			p.pos++
		}
		if p.pos >= len(p.src) {
			return "", fmt.Errorf("unterminated string")
		}
		p.pos++
		return sb.String(), nil
	}
	start := p.pos
	for p.pos < len(p.src) && !strings.ContainsRune(" \t\r\n{}\"", rune(p.src[p.pos])) {
		p.pos++
	}
	if start == p.pos {
		return "", fmt.Errorf("unexpected %q at %d", p.src[p.pos], p.pos)
	}
	return p.src[start:p.pos], nil
}

func (p *vdfParser) value() (any, error) {
	p.skipSpace()
	if p.pos < len(p.src) && p.src[p.pos] == '{' {
		p.pos++
		obj := map[string]any{}
		for {
			p.skipSpace()
			if p.pos < len(p.src) && p.src[p.pos] == '}' {
				p.pos++
				return obj, nil
			}
			key, err := p.token()
			if err != nil {
				return nil, err
			}
			val, err := p.value()
			if err != nil {
				return nil, err
			}
			obj[key] = val
		}
	}
	return p.token()
}
