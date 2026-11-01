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
	"syscall"
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

// guestCall runs one guest-agent command in its own short connection, under a
// per-VM lock, so any number of processes can share a VM. Each connection
// starts with a delimited sync, which throws away anything a previous client
// left unread.
func guestCall(id, cmd string, args any, out any, timeout time.Duration) error {
	touch(id)
	return agentCall(vmDir(id), cmd, args, out, timeout)
}

func agentCall(dir, cmd string, args any, out any, timeout time.Duration) error {
	lock, err := os.OpenFile(filepath.Join(dir, "qga.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("vm not found: %s", filepath.Base(dir))
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	c, err := net.DialTimeout("unix", filepath.Join(dir, "qga.sock"), 5*time.Second)
	if err != nil {
		return fmt.Errorf("guest agent unreachable: %w", err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(timeout))

	sync := rand.Int64N(1 << 40)
	msg := fmt.Sprintf("\xff{\"execute\":\"guest-sync-delimited\",\"arguments\":{\"id\":%d}}\n", sync)
	if _, err := io.WriteString(c, msg); err != nil {
		return err
	}
	lr := &limitedReader{r: c, n: maxReply}
	br := bufio.NewReaderSize(lr, 64<<10)
	for {
		if _, err := br.ReadBytes(0xff); err != nil {
			return fmt.Errorf("guest agent sync: %w", err)
		}
		dec := json.NewDecoder(br)
		var r reply
		if err := dec.Decode(&r); err != nil {
			continue
		}
		var got int64
		if json.Unmarshal(r.Return, &got) == nil && got == sync {
			br = bufio.NewReaderSize(io.MultiReader(dec.Buffered(), br), 64<<10)
			break
		}
	}
	req, _ := json.Marshal(map[string]any{"execute": cmd, "arguments": args})
	if args == nil {
		req, _ = json.Marshal(map[string]any{"execute": cmd})
	}
	if _, err := c.Write(append(req, '\n')); err != nil {
		return err
	}
	lr.n = maxReply
	dec := json.NewDecoder(br)
	var r reply
	if err := dec.Decode(&r); err != nil {
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

// touch records activity so the idle reaper leaves the VM alone.
func touch(id string) {
	now := time.Now()
	os.Chtimes(filepath.Join(vmDir(id), "activity"), now, now)
}

func guestPing(id string, timeout time.Duration) error {
	return guestCall(id, "guest-ping", nil, nil, timeout)
}

// qmp sends one command to QEMU's monitor.
func qmp(dir, cmd string, args any, timeout time.Duration) (json.RawMessage, error) {
	c, err := net.DialTimeout("unix", filepath.Join(dir, "qmp.sock"), 3*time.Second)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(timeout))
	dec := json.NewDecoder(c)
	var greet map[string]any
	if err := dec.Decode(&greet); err != nil {
		return nil, err
	}
	send := func(cmd string, args any) (json.RawMessage, error) {
		m := map[string]any{"execute": cmd}
		if args != nil {
			m["arguments"] = args
		}
		b, _ := json.Marshal(m)
		if _, err := c.Write(b); err != nil {
			return nil, err
		}
		for {
			var r struct {
				reply
				Event string `json:"event"`
			}
			if err := dec.Decode(&r); err != nil {
				return nil, err
			}
			if r.Event != "" {
				continue
			}
			if r.Error != nil {
				return nil, r.Error
			}
			return r.Return, nil
		}
	}
	if _, err := send("qmp_capabilities", nil); err != nil {
		return nil, err
	}
	return send(cmd, args)
}
