// Package gate is a TCP front door for games whose server cannot survive a
// connection that opens and drops before saying anything (vanilla Terraria
// dies of an ObjectDisposedException in exactly that case). It accepts on the
// public port and forwards a client to the server only once the client has
// sent its first bytes — optionally required to contain a marker, such as
// the "Terraria" of a connect request — within a short window. Everything
// else is closed at the door and never reaches the game.
package gate

import (
	"bytes"
	"context"
	"io"
	"net"
	"sync"
	"time"

	"github.com/Reclyptor/GameOps/internal/logx"
)

type Gate struct {
	Listen  string        // e.g. ":7777"
	Target  string        // e.g. "127.0.0.1:7778"
	Expect  string        // marker the first bytes must contain; empty = any bytes
	Timeout time.Duration // how long a client has to send them
	// OnPass / OnDrop, when set, count for /metrics.
	OnPass, OnDrop func()
}

// Serve accepts until ctx is done.
func (g *Gate) Serve(ctx context.Context) error {
	ln, err := net.Listen("tcp", g.Listen)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	logx.Infof("gate on %s → %s (clients must send %s within %s)", g.Listen, g.Target, g.describe(), g.Timeout)
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go g.handle(c)
	}
}

func (g *Gate) describe() string {
	if g.Expect == "" {
		return "their first bytes"
	}
	return "a " + g.Expect + " connect request"
}

func (g *Gate) handle(c net.Conn) {
	defer c.Close()
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	buf := make([]byte, 256)
	c.SetReadDeadline(time.Now().Add(g.Timeout))
	n, err := c.Read(buf)
	c.SetReadDeadline(time.Time{})
	if err != nil || n == 0 || (g.Expect != "" && !bytes.Contains(buf[:n], []byte(g.Expect))) {
		logx.Debugf("gate: dropped %s (%d bytes before the deadline)", c.RemoteAddr(), n)
		if g.OnDrop != nil {
			g.OnDrop()
		}
		return
	}
	up, err := net.DialTimeout("tcp", g.Target, 5*time.Second)
	if err != nil {
		logx.Debugf("gate: server not accepting (%v); dropped %s", err, c.RemoteAddr())
		if g.OnDrop != nil {
			g.OnDrop()
		}
		return
	}
	defer up.Close()
	if _, err := up.Write(buf[:n]); err != nil {
		return
	}
	if g.OnPass != nil {
		g.OnPass()
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); io.Copy(up, c); halfClose(up) }()
	go func() { defer wg.Done(); io.Copy(c, up); halfClose(c) }()
	wg.Wait()
}

func halfClose(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		tc.CloseWrite()
	}
}
