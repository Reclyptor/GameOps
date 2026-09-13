// Package rcon is a Source-RCON client: authenticate, send a command, read
// the (possibly multi-packet) reply. Used by the `gameops rcon` subcommand
// and by adapters through the shim's rcon() helper.
package rcon

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

const (
	typeResponse = 0
	typeExec     = 2
	typeAuth     = 3

	maxPacket = 4096 + 10
)

var ErrAuth = errors.New("rcon: authentication failed")

type Client struct {
	conn net.Conn
	id   int32
}

// Dial connects and authenticates.
func Dial(addr, password string, timeout time.Duration) (*Client, error) {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, err
	}
	c := &Client{conn: conn}
	conn.SetDeadline(time.Now().Add(timeout))
	if err := c.write(typeAuth, password); err != nil {
		conn.Close()
		return nil, err
	}
	// Some servers send an empty response packet before the auth reply.
	for i := 0; i < 2; i++ {
		id, typ, _, err := c.read()
		if err != nil {
			conn.Close()
			return nil, err
		}
		if typ == typeExec || typ == typeAuth {
			if id == -1 {
				conn.Close()
				return nil, ErrAuth
			}
			return c, nil
		}
	}
	conn.Close()
	return nil, errors.New("rcon: no auth reply")
}

func (c *Client) Close() error { return c.conn.Close() }

// Exec sends one command and returns the full reply body. Replies longer than
// one packet arrive as several; they are read until the server goes quiet.
func (c *Client) Exec(command string, timeout time.Duration) (string, error) {
	c.conn.SetDeadline(time.Now().Add(timeout))
	if err := c.write(typeExec, command); err != nil {
		return "", err
	}
	var body bytes.Buffer
	first := true
	for {
		if !first {
			c.conn.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
		}
		_, typ, b, err := c.read()
		if err != nil {
			if !first && isTimeout(err) {
				break
			}
			if !first && errors.Is(err, io.EOF) {
				break
			}
			return "", err
		}
		first = false
		if typ != typeResponse {
			continue
		}
		body.Write(b)
		if len(b) < 4096 {
			// A full 4096-byte body means more may follow; anything shorter
			// is the end. Wait briefly for stragglers anyway.
			c.conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			if _, _, more, err := c.read(); err == nil {
				body.Write(more)
				continue
			}
			break
		}
	}
	return body.String(), nil
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func (c *Client) write(typ int32, body string) error {
	c.id++
	buf := new(bytes.Buffer)
	size := int32(4 + 4 + len(body) + 2)
	binary.Write(buf, binary.LittleEndian, size)
	binary.Write(buf, binary.LittleEndian, c.id)
	binary.Write(buf, binary.LittleEndian, typ)
	buf.WriteString(body)
	buf.Write([]byte{0, 0})
	_, err := c.conn.Write(buf.Bytes())
	return err
}

func (c *Client) read() (id, typ int32, body []byte, err error) {
	var size int32
	if err = binary.Read(c.conn, binary.LittleEndian, &size); err != nil {
		return
	}
	if size < 10 || size > maxPacket {
		err = fmt.Errorf("rcon: bad packet size %d", size)
		return
	}
	pkt := make([]byte, size)
	if _, err = io.ReadFull(c.conn, pkt); err != nil {
		return
	}
	id = int32(binary.LittleEndian.Uint32(pkt[0:4]))
	typ = int32(binary.LittleEndian.Uint32(pkt[4:8]))
	body = bytes.TrimRight(pkt[8:], "\x00")
	return
}

// Command is the one-shot form: dial, exec, close.
func Command(addr, password, command string, timeout time.Duration) (string, error) {
	c, err := Dial(addr, password, timeout)
	if err != nil {
		return "", err
	}
	defer c.Close()
	return c.Exec(command, timeout)
}
