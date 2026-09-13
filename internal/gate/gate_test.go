package gate

import (
	"context"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// echoServer stands in for the game: it echoes what it receives and counts
// the connections that reached it.
func echoServer(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var reached atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			reached.Add(1)
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return ln.Addr().String(), &reached
}

func start(t *testing.T, target, expect string) (string, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	var pass, drop atomic.Int32
	g := &Gate{Listen: addr, Target: target, Expect: expect, Timeout: 300 * time.Millisecond,
		OnPass: func() { pass.Add(1) }, OnDrop: func() { drop.Add(1) }}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go g.Serve(ctx)
	for i := 0; i < 50; i++ {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(350 * time.Millisecond) // let that probe be dropped and counted
	drop.Store(0)
	return addr, &pass, &drop
}

func TestGate(t *testing.T) {
	target, reached := echoServer(t)
	addr, pass, drop := start(t, target, "Terraria")

	// A bare connect-and-close never reaches the server.
	c, _ := net.Dial("tcp", addr)
	c.Close()
	// A connection that says nothing is closed at the deadline.
	silent, _ := net.Dial("tcp", addr)
	// Wrong first bytes are dropped too.
	wrong, _ := net.Dial("tcp", addr)
	wrong.Write([]byte("GET / HTTP/1.0\r\n\r\n"))
	time.Sleep(500 * time.Millisecond)
	if n := reached.Load(); n != 0 {
		t.Fatalf("%d connections reached the server; none should have", n)
	}
	if buf := make([]byte, 1); func() bool {
		silent.SetReadDeadline(time.Now().Add(time.Second))
		_, err := silent.Read(buf)
		return err == io.EOF
	}() {
		// closed by the gate, as expected
	} else {
		t.Error("a silent connection should be closed by the gate")
	}
	silent.Close()
	wrong.Close()

	// A real connect request is forwarded, first bytes included, both ways.
	good, _ := net.Dial("tcp", addr)
	req := []byte("\x0c\x00\x01\x0bTerraria279")
	good.Write(req)
	buf := make([]byte, len(req))
	good.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(good, buf); err != nil || string(buf) != string(req) {
		t.Fatalf("echo through the gate failed: %q %v", buf, err)
	}
	good.Write([]byte("more"))
	buf = make([]byte, 4)
	if _, err := io.ReadFull(good, buf); err != nil || string(buf) != "more" {
		t.Fatalf("later traffic not relayed: %q %v", buf, err)
	}
	good.Close()
	time.Sleep(100 * time.Millisecond)
	if reached.Load() != 1 || pass.Load() != 1 || drop.Load() != 3 {
		t.Fatalf("reached=%d pass=%d drop=%d", reached.Load(), pass.Load(), drop.Load())
	}
}

func TestGateWithoutMarkerForwardsAnyFirstBytes(t *testing.T) {
	target, reached := echoServer(t)
	addr, _, _ := start(t, target, "")
	c, _ := net.Dial("tcp", addr)
	c.Write([]byte("hello"))
	buf := make([]byte, 5)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "hello" {
		t.Fatalf("got %q %v", buf, err)
	}
	c.Close()
	if reached.Load() != 1 {
		t.Fatalf("reached=%d", reached.Load())
	}
}
