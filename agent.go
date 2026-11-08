package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"time"
)

// The guest is untrusted: cap how much one reply may make us buffer.
const maxReply = 96 << 20

type limitedReader struct {
	r io.Reader
	n int64
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.n <= 0 {
		return 0, errors.New("guest reply too large")
	}
	if int64(len(p)) > l.n {
		p = p[:l.n]
	}
	n, err := l.r.Read(p)
	l.n -= int64(n)
	return n, err
}

type rpcError struct {
	Class string `json:"class"`
	Desc  string `json:"desc"`
}

func (e *rpcError) Error() string { return e.Desc }

type reply struct {
	Return json.RawMessage `json:"return"`
	Error  *rpcError       `json:"error"`
}

// agentHold is how long one process may keep a VM's agent connection before
// handing it back. The guest agent idles ~100 ms after every disconnect, so a
// burst of calls is much faster over one connection, but other processes
// waiting on the same VM must still get a turn.
const agentHold = 500 * time.Millisecond

// agentConn is a connection to a VM's guest agent, held under the VM's lock.
// It dials on first use and redials after an error or after agentHold.
type agentConn struct {
	dir   string
	id    string // if set, every call counts as activity for the idle reaper
	lock  *os.File
	c     net.Conn
	br    *bufio.Reader
	lr    *limitedReader
	since time.Time
}

func guestSession(id string) *agentConn { return &agentConn{dir: vmDir(id), id: id} }

func (a *agentConn) dial() error {
	lock, err := lockFile(filepath.Join(a.dir, "qga.lock"), true)
	if err != nil {
		return fmt.Errorf("vm not found: %s", filepath.Base(a.dir))
	}
	// Through the supervisor's proxy when there is one (see agentProxy).
	sock := filepath.Join(a.dir, "agent.sock")
	if !exists(sock) {
		sock = qgaSock(a.dir)
	}
	c, err := net.DialTimeout("unix", sock, 5*time.Second)
	if err != nil {
		lock.Close()
		return fmt.Errorf("guest agent unreachable: %w", err)
	}
	a.lock, a.c, a.since = lock, c, time.Now()
	a.lr = &limitedReader{r: c, n: maxReply}
	a.br = bufio.NewReaderSize(a.lr, 64<<10)
	return nil
}

func (a *agentConn) close() {
	if a.c != nil {
		a.c.Close()
		a.lock.Close()
		a.c, a.lock = nil, nil
	}
}

// call runs one command. Each call starts with a delimited sync, which throws
// away anything a previous client (or a timed-out call) left unread.
func (a *agentConn) call(cmd string, args any, out any, timeout time.Duration) error {
	if a.id != "" {
		touch(a.id)
	}
	if a.c != nil && time.Since(a.since) > agentHold {
		a.close()
	}
	if a.c == nil {
		if err := a.dial(); err != nil {
			return err
		}
	}
	err := a.roundTrip(cmd, args, out, timeout)
	if err != nil {
		a.close()
	}
	return err
}

func (a *agentConn) roundTrip(cmd string, args any, out any, timeout time.Duration) error {
	a.c.SetDeadline(time.Now().Add(timeout))
	sync := rand.Int64N(1 << 40)
	msg := fmt.Sprintf("\xff{\"execute\":\"guest-sync-delimited\",\"arguments\":{\"id\":%d}}\n", sync)
	if _, err := io.WriteString(a.c, msg); err != nil {
		return err
	}
	a.lr.n = maxReply
	for {
		if _, err := a.br.ReadBytes(0xff); err != nil {
			return fmt.Errorf("guest agent sync: %w", err)
		}
		line, err := a.br.ReadBytes('\n')
		if err != nil {
			return fmt.Errorf("guest agent sync: %w", err)
		}
		var r reply
		var got int64
		if json.Unmarshal(line, &r) == nil && json.Unmarshal(r.Return, &got) == nil && got == sync {
			break
		}
	}
	req, _ := json.Marshal(map[string]any{"execute": cmd, "arguments": args})
	if args == nil {
		req, _ = json.Marshal(map[string]any{"execute": cmd})
	}
	if _, err := a.c.Write(append(req, '\n')); err != nil {
		return err
	}
	a.lr.n = maxReply
	line, err := a.br.ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("%s: %w", cmd, err)
	}
	var r reply
	if err := json.Unmarshal(line, &r); err != nil {
		return fmt.Errorf("%s: %w", cmd, err)
	}
	if r.Error != nil {
		return fmt.Errorf("%s: %w", cmd, r.Error)
	}
	if out != nil {
		return json.Unmarshal(r.Return, out)
	}
	return nil
}

// agentProxy keeps the one connection to the guest agent open for the VM's
// whole life and lends it to one client at a time. The agent pauses ~100 ms
// every time the host side hangs up; through the proxy it never sees that.
// Clients sync at the start of every call, so whatever one leaves unread is
// thrown away by the next.
func agentProxy(dir string) error {
	tmp := filepath.Join(dir, "agent.sock.tmp")
	os.Remove(tmp)
	ln, err := net.Listen("unix", tmp)
	if err != nil {
		return err
	}
	// Connect before the guest runs: a restored guest that finds the host side
	// closed would start its pause loop.
	var qga net.Conn
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(2 * time.Millisecond) {
		if qga, err = net.Dial("unix", qgaSock(dir)); err == nil {
			break
		}
		if time.Now().After(deadline) {
			ln.Close()
			return err
		}
	}
	if err := os.Rename(tmp, filepath.Join(dir, "agent.sock")); err != nil {
		ln.Close()
		qga.Close()
		return err
	}
	go func() {
		for {
			cl, err := ln.Accept()
			if err != nil {
				return
			}
			if qga == nil {
				if qga, err = net.DialTimeout("unix", qgaSock(dir), 5*time.Second); err != nil {
					qga = nil
					cl.Close()
					continue
				}
			}
			if !lend(cl, qga) {
				qga.Close()
				qga = nil
			}
		}
	}()
	return nil
}

// lend pipes one client to the agent until the client hangs up. It reports
// whether the agent connection is still good.
func lend(cl, qga net.Conn) bool {
	defer cl.Close()
	back := make(chan error, 1)
	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, err := qga.Read(buf)
			if n > 0 {
				cl.Write(buf[:n]) // a client that went away just misses the rest
			}
			if err != nil {
				back <- err
				return
			}
		}
	}()
	// A guest that stops reading must not wedge the proxy for good: no call
	// waits longer than this for one write.
	writeFailed := false
	buf := make([]byte, 64<<10)
	for {
		n, err := cl.Read(buf)
		if n > 0 {
			qga.SetWriteDeadline(time.Now().Add(2 * time.Minute))
			if _, werr := qga.Write(buf[:n]); werr != nil {
				writeFailed = true
				break
			}
		}
		if err != nil {
			break
		}
	}
	qga.SetReadDeadline(time.Now()) // stop the reader, keep the connection
	rerr := <-back
	qga.SetReadDeadline(time.Time{})
	return !writeFailed && errors.Is(rerr, os.ErrDeadlineExceeded)
}

// touch records activity so the idle reaper leaves the VM alone.
func touch(id string) {
	now := time.Now()
	os.Chtimes(filepath.Join(vmDir(id), fileActivity), now, now)
}
