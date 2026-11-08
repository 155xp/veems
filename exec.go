package main

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"
)

type ExecOpts struct {
	Argv    []string // run this program directly
	Line    string   // or run this through the shell (bash on Linux, PowerShell on Windows)
	Cwd     string
	Env     []string
	Stdin   []byte
	Timeout time.Duration // 0 = none
}

type ExecResult struct {
	ExitCode  int
	Stdout    []byte
	Stderr    []byte
	TimedOut  bool
	Truncated bool
}

type execStatus struct {
	Exited       bool   `json:"exited"`
	ExitCode     *int   `json:"exitcode"`
	Signal       *int   `json:"signal"`
	OutData      string `json:"out-data"`
	ErrData      string `json:"err-data"`
	OutTruncated bool   `json:"out-truncated"`
	ErrTruncated bool   `json:"err-truncated"`
}

func (st *execStatus) result() *ExecResult {
	r := &ExecResult{Truncated: st.OutTruncated || st.ErrTruncated}
	r.Stdout, _ = base64.StdEncoding.DecodeString(st.OutData)
	r.Stderr, _ = base64.StdEncoding.DecodeString(st.ErrData)
	switch {
	case st.ExitCode != nil:
		r.ExitCode = *st.ExitCode
	case st.Signal != nil:
		r.ExitCode = 128 + *st.Signal
	}
	return r
}

func (a *agentConn) start(p guestProc, capture bool) (int, error) {
	req := map[string]any{"path": p.Path, "arg": p.Args, "capture-output": capture}
	if p.Env != nil {
		req["env"] = p.Env
	}
	if p.Input != nil {
		req["input-data"] = base64.StdEncoding.EncodeToString(p.Input)
	}
	var started struct {
		PID int `json:"pid"`
	}
	err := a.call("guest-exec", req, &started, 60*time.Second)
	return started.PID, err
}

// wait polls pid until it exits. It returns nil if deadline (unless zero)
// passes first.
func (a *agentConn) wait(pid int, deadline time.Time) (*execStatus, error) {
	delay := 2 * time.Millisecond
	for {
		var st execStatus
		if err := a.call("guest-exec-status", map[string]any{"pid": pid}, &st, 120*time.Second); err != nil {
			return nil, err
		}
		if st.Exited {
			return &st, nil
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return nil, nil
		}
		if interrupted() {
			return nil, errInterrupted
		}
		time.Sleep(delay)
		delay = min(delay*3/2, 200*time.Millisecond)
	}
}

// run starts p and waits up to limit for it, ignoring how it went.
func (a *agentConn) run(p guestProc, limit time.Duration) {
	if pid, err := a.start(p, false); err == nil {
		a.wait(pid, time.Now().Add(limit))
	}
}

func guestExec(m *Meta, o ExecOpts) (*ExecResult, error) {
	// One connection for the whole exec: short commands finish before the
	// session hands the agent back (see agentHold).
	a := guestSession(m.ID)
	defer a.close()
	p, err := m.guest().proc(a, o)
	if err != nil {
		return nil, err
	}
	p.Input = o.Stdin
	pid, err := a.start(p, true)
	if err != nil {
		return nil, err
	}
	defer onInterrupt(func() {
		k := guestSession(m.ID)
		k.start(m.guest().killTree(pid), false) // the process is exiting: no waiting for it
		k.close()
	})()
	var deadline time.Time
	if o.Timeout > 0 {
		deadline = time.Now().Add(o.Timeout)
	}
	st, err := a.wait(pid, deadline)
	if err != nil {
		return nil, err
	}
	if st == nil {
		// Whatever the command printed before it was killed is still worth having.
		a.run(m.guest().killTree(pid), 3*time.Second)
		r := &ExecResult{ExitCode: 124, TimedOut: true}
		if st, _ := a.wait(pid, time.Now().Add(5*time.Second)); st != nil {
			out := st.result()
			r.Stdout, r.Stderr, r.Truncated = out.Stdout, out.Stderr, out.Truncated
		}
		return r, nil
	}
	return st.result(), nil
}

func mustExec(m *Meta, o ExecOpts) (string, error) {
	r, err := guestExec(m, o)
	if err != nil {
		return "", err
	}
	if r.ExitCode != 0 {
		return "", fmt.Errorf("%s", strings.TrimSpace(string(r.Stderr)+"\n"+string(r.Stdout)))
	}
	return string(r.Stdout), nil
}
