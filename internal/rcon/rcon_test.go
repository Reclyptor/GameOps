package rcon

import (
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeServer speaks just enough Source RCON to test the client: it accepts
// one password, echoes commands, and splits one reply across two packets.
func fakeServer(t *testing.T, password string) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				authed := false
				for {
					var size int32
					if err := binary.Read(c, binary.LittleEndian, &size); err != nil {
						return
					}
					pkt := make([]byte, size)
					if _, err := io.ReadFull(c, pkt); err != nil {
						return
					}
					id := int32(binary.LittleEndian.Uint32(pkt[0:4]))
					typ := int32(binary.LittleEndian.Uint32(pkt[4:8]))
					body := strings.TrimRight(string(pkt[8:]), "\x00")
					write := func(id, typ int32, body string) {
						out := make([]byte, 0, 14+len(body))
						out = binary.LittleEndian.AppendUint32(out, uint32(10+len(body)))
						out = binary.LittleEndian.AppendUint32(out, uint32(id))
						out = binary.LittleEndian.AppendUint32(out, uint32(typ))
						out = append(out, body...)
						out = append(out, 0, 0)
						c.Write(out)
					}
					switch typ {
					case typeAuth:
						write(id, typeResponse, "")
						if body == password {
							authed = true
							write(id, typeExec, "")
						} else {
							write(-1, typeExec, "")
						}
					case typeExec:
						if !authed {
							return
						}
						if body == "long" {
							write(id, typeResponse, strings.Repeat("a", 4096))
							write(id, typeResponse, "tail")
						} else {
							write(id, typeResponse, "echo: "+body)
						}
					}
				}
			}(conn)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return ln.Addr().String()
}

func TestCommand(t *testing.T) {
	addr := fakeServer(t, "secret")
	out, err := Command(addr, "secret", "/version", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if out != "echo: /version" {
		t.Fatalf("got %q", out)
	}
}

func TestMultiPacket(t *testing.T) {
	addr := fakeServer(t, "secret")
	out, err := Command(addr, "secret", "long", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 4100 || !strings.HasSuffix(out, "tail") {
		t.Fatalf("got %d bytes, suffix %q", len(out), out[max(0, len(out)-4):])
	}
}

func TestBadPassword(t *testing.T) {
	addr := fakeServer(t, "secret")
	if _, err := Command(addr, "wrong", "x", 2*time.Second); err != ErrAuth {
		t.Fatalf("expected ErrAuth, got %v", err)
	}
}
