// Package httpx is the toolkit's HTTP client: GET with retries and sane
// timeouts, to memory or to a file. Adapters use it through the shim's
// http_get helper, so game images need no curl.
package httpx

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

var client = &http.Client{Timeout: 10 * time.Minute}

const userAgent = "gameops"

func get(url string) (*http.Response, error) {
	var last error
	for attempt := 1; attempt <= 3; attempt++ {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", userAgent)
		resp, err := client.Do(req)
		if err == nil && resp.StatusCode < 400 {
			return resp, nil
		}
		if err == nil {
			resp.Body.Close()
			err = fmt.Errorf("HTTP %d", resp.StatusCode)
			if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != 429 {
				return nil, fmt.Errorf("GET %s: %w", url, err)
			}
		}
		last = err
		time.Sleep(time.Duration(attempt) * 2 * time.Second)
	}
	return nil, fmt.Errorf("GET %s: %w", url, last)
}

// Get returns the body of a successful GET (up to 64 MiB).
func Get(url string) ([]byte, error) {
	resp, err := get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	return b, nil
}

// Download streams a GET to path via a temporary file and an atomic rename.
func Download(url, path string) error {
	resp, err := get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// ErrNotFound distinguishes a 404 for callers that treat it as "no such thing".
var ErrNotFound = errors.New("not found")
