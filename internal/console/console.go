// Package console is the server's stdin pipe: a FIFO the runner holds open
// read-write for its own lifetime, so the server never sees EOF when a writer
// closes and writers never block after the server has gone.
package console

import (
	"fmt"
	"os"
	"syscall"
)

type Console struct {
	Path string
	keep *os.File
}

func Open(path string) (*Console, error) {
	os.Remove(path)
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		return nil, fmt.Errorf("cannot create console FIFO at %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	return &Console{Path: path, keep: f}, nil
}

// Stdin is the file the server process reads from.
func (c *Console) Stdin() *os.File { return c.keep }

func (c *Console) Close() error {
	err := c.keep.Close()
	os.Remove(c.Path)
	return err
}

// Send writes one line to the FIFO. Opening write-only and non-blocking
// fails fast when no runner holds the pipe (no server running).
func Send(path, line string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("console is not open; is the server running? (%w)", err)
	}
	defer f.Close()
	_, err = f.WriteString(line + "\n")
	return err
}
