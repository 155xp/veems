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
	ExitCode  int    `json:"exit_code"`
	Stdout    []byte `json:"-"`
	Stderr    []byte `json:"-"`
	TimedOut  bool   `json:"timed_out,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
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

const linuxPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// psQuote makes a PowerShell single-quoted string literal.
func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// guestCommand turns ExecOpts into the program, arguments, and environment
// handed to the guest agent.
func guestCommand(m *Meta, o ExecOpts) (path string, args, env []string) {
	p, _ := profile(m.OS)
	cwd := o.Cwd
	if cwd == "" {
		cwd = p.WorkDir
	}
	if m.OS == "windows" {
		return "powershell.exe", nil, nil // script set up by guestExec
	}
	env = append([]string{"HOME=/root", "USER=root", "PATH=" + linuxPath, "LANG=C.UTF-8", "TERM=dumb"}, o.Env...)
	cmd := o.Argv
	if o.Line != "" {
		cmd = []string{"/bin/bash", "-c", o.Line}
	}
	args = append([]string{"-c", `cd -- "$1" || exit 125; shift; exec "$@"`, "veems", cwd}, cmd...)
	return "/bin/sh", args, env
}

// psScript wraps a Windows command so errors print as plain text and the
// exit code of the last native program (or 1 on a PowerShell error) comes back.
func psScript(m *Meta, o ExecOpts) string {
	p, _ := profile(m.OS)
	cwd := o.Cwd
	if cwd == "" {
		cwd = p.WorkDir
	}
	var s strings.Builder
	s.WriteString("Remove-Item -LiteralPath $PSCommandPath -Force -ErrorAction SilentlyContinue\n")
	s.WriteString("$ProgressPreference='SilentlyContinue'\n[Console]::OutputEncoding=[Text.Encoding]::UTF8\n")
	for _, kv := range o.Env {
		k, v, _ := strings.Cut(kv, "=")
		fmt.Fprintf(&s, "Set-Item -LiteralPath %s -Value %s\n", psQuote("env:"+k), psQuote(v))
	}
	fmt.Fprintf(&s, "Set-Location -LiteralPath %s\n$global:LASTEXITCODE=$null\n", psQuote(cwd))
	if o.Line != "" {
		s.WriteString("& {\n" + o.Line + "\n}\n")
	} else {
		s.WriteString("&")
		for _, a := range o.Argv {
			s.WriteString(" " + psQuote(a))
		}
		s.WriteString("\n")
	}
	s.WriteString("$veemsOK=$?\nif ($null -ne $LASTEXITCODE) { exit $LASTEXITCODE }\nif (-not $veemsOK) { exit 1 }\nexit 0\n")
	return s.String()
}

func guestExec(m *Meta, o ExecOpts) (*ExecResult, error) {
	path, args, env := guestCommand(m, o)
	if m.OS == "windows" {
		// -EncodedCommand would turn stderr into CLIXML, so run a script file
		// (it deletes itself first thing). UTF-8 with BOM so non-ASCII survives.
		script := fmt.Sprintf(`C:\Windows\Temp\veems-%d.ps1`, time.Now().UnixNano())
		if err := writeGuest(m, script, strings.NewReader("\ufeff"+psScript(m, o))); err != nil {
			return nil, err
		}
		args = []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script}
	}
	req := map[string]any{"path": path, "arg": args, "capture-output": true}
	if env != nil {
		req["env"] = env
	}
	if o.Stdin != nil {
		req["input-data"] = base64.StdEncoding.EncodeToString(o.Stdin)
	}
	var started struct {
		PID int `json:"pid"`
	}
	if err := guestCall(m.ID, "guest-exec", req, &started, 60*time.Second); err != nil {
		return nil, err
	}
	var deadline time.Time
	if o.Timeout > 0 {
		deadline = time.Now().Add(o.Timeout)
	}
	delay := 5 * time.Millisecond
	for {
		var st execStatus
		if err := guestCall(m.ID, "guest-exec-status", map[string]any{"pid": started.PID}, &st, 120*time.Second); err != nil {
			return nil, err
		}
		if st.Exited {
			r := &ExecResult{Truncated: st.OutTruncated || st.ErrTruncated}
			r.Stdout, _ = base64.StdEncoding.DecodeString(st.OutData)
			r.Stderr, _ = base64.StdEncoding.DecodeString(st.ErrData)
			switch {
			case st.ExitCode != nil:
				r.ExitCode = *st.ExitCode
			case st.Signal != nil:
				r.ExitCode = 128 + *st.Signal
			}
			return r, nil
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			killGuestTree(m, started.PID)
			return &ExecResult{ExitCode: 124, TimedOut: true}, nil
		}
		time.Sleep(delay)
		if delay < 200*time.Millisecond {
			delay = delay * 3 / 2
		}
	}
}

func killGuestTree(m *Meta, pid int) {
	var path string
	var args []string
	if m.OS == "windows" {
		path, args = "taskkill.exe", []string{"/F", "/T", "/PID", fmt.Sprint(pid)}
	} else {
		path, args = "/bin/sh", []string{"-c", `kt(){ for c in $(pgrep -P "$1"); do kt "$c"; done; kill -9 "$1"; }; kt "$1"`, "veems", fmt.Sprint(pid)}
	}
	var started struct {
		PID int `json:"pid"`
	}
	if guestCall(m.ID, "guest-exec", map[string]any{"path": path, "arg": args}, &started, 10*time.Second) == nil {
		var st execStatus
		for i := 0; i < 50; i++ {
			if guestCall(m.ID, "guest-exec-status", map[string]any{"pid": started.PID}, &st, 10*time.Second) != nil || st.Exited {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	var st execStatus
	guestCall(m.ID, "guest-exec-status", map[string]any{"pid": pid}, &st, 10*time.Second) // reap
}

// mustExec runs a helper command in the guest and fails on a nonzero exit.
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
