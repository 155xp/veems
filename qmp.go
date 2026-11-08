package main

import (
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"time"
)

type qmpConn struct {
	c   net.Conn
	dec *json.Decoder
}

func dialQMP(dir string, timeout time.Duration) (*qmpConn, error) {
	c, err := net.DialTimeout("unix", filepath.Join(dir, "qmp.sock"), 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("vm monitor unreachable: %w", err)
	}
	q := &qmpConn{c: c, dec: json.NewDecoder(c)}
	c.SetDeadline(time.Now().Add(timeout))
	var greet map[string]any
	if err := q.dec.Decode(&greet); err != nil {
		c.Close()
		return nil, err
	}
	if _, err := q.do("qmp_capabilities", nil, timeout); err != nil {
		c.Close()
		return nil, err
	}
	return q, nil
}

func (q *qmpConn) do(cmd string, args any, timeout time.Duration) (json.RawMessage, error) {
	q.c.SetDeadline(time.Now().Add(timeout))
	m := map[string]any{"execute": cmd}
	if args != nil {
		m["arguments"] = args
	}
	b, _ := json.Marshal(m)
	if _, err := q.c.Write(b); err != nil {
		return nil, err
	}
	for {
		var r struct {
			reply
			Event string `json:"event"`
		}
		if err := q.dec.Decode(&r); err != nil {
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

func (q *qmpConn) Close() { q.c.Close() }

func qmp(dir, cmd string, args any, timeout time.Duration) (json.RawMessage, error) {
	q, err := dialQMP(dir, timeout)
	if err != nil {
		return nil, err
	}
	defer q.Close()
	return q.do(cmd, args, timeout)
}
