package main

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"
	"unicode/utf16"
)

type windowsGuest struct{}

func (windowsGuest) workDir() string { return `C:\work` }

// proc runs o from a script file: -EncodedCommand would turn stderr into
// CLIXML. The file is UTF-8 with a BOM so non-ASCII survives, and it deletes
// itself first thing.
func (g windowsGuest) proc(a *agentConn, o ExecOpts) (guestProc, error) {
	script := g.temp(".ps1")
	body := "\ufeffRemove-Item -LiteralPath $PSCommandPath -Force -ErrorAction SilentlyContinue\n" + psScript(cwdOf(g, o), o)
	if err := writeFile(a, script, strings.NewReader(body)); err != nil {
		return guestProc{}, err
	}
	return guestProc{Path: "powershell.exe",
		Args: []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script}}, nil
}

// psQuote makes a PowerShell single-quoted string literal.
func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// psScript wraps a command so errors print as plain text and, like bash, the
// last command decides the exit code: 0 if it succeeded, else the native
// program's exit code, or 1 for a failed cmdlet.
func psScript(cwd string, o ExecOpts) string {
	var s strings.Builder
	s.WriteString("$ProgressPreference='SilentlyContinue'\n[Console]::OutputEncoding=[Text.Encoding]::UTF8\n")
	for _, kv := range o.Env {
		k, v, _ := strings.Cut(kv, "=")
		fmt.Fprintf(&s, "Set-Item -LiteralPath %s -Value %s\n", psQuote("env:"+k), psQuote(v))
	}
	fmt.Fprintf(&s, "Set-Location -LiteralPath %s\n$global:LASTEXITCODE=$null\n", psQuote(cwd))
	if o.Line != "" {
		s.WriteString(o.Line + "\n")
	} else {
		s.WriteString("&")
		for _, a := range o.Argv {
			s.WriteString(" " + psQuote(a))
		}
		s.WriteString("\n")
	}
	s.WriteString("if ($?) { exit 0 }\nif ($LASTEXITCODE) { exit $LASTEXITCODE }\nexit 1\n")
	return s.String()
}

func (windowsGuest) killTree(pid int) guestProc {
	return guestProc{Path: "taskkill.exe", Args: []string{"/F", "/T", "/PID", fmt.Sprint(pid)}}
}

func (g windowsGuest) abs(p string) string {
	p = strings.ReplaceAll(p, "/", `\`)
	switch {
	case p == "" || p == ".":
		return g.workDir()
	case len(p) >= 2 && p[1] == ':' || strings.HasPrefix(p, `\\`):
		return p
	}
	return g.workDir() + `\` + p
}

func (windowsGuest) base(p string) string {
	p = strings.TrimRight(p, `\`)
	return p[strings.LastIndexAny(p, `\:`)+1:]
}

func (windowsGuest) join(dir, name string) string { return strings.TrimRight(dir, `\`) + `\` + name }
func (windowsGuest) temp(ext string) string       { return tempName(`C:\Windows\Temp`, `\`, ext) }

// The Windows guest agent stalls for good when one reply is much over 512 KB.
func (windowsGuest) readChunk() int { return 256 << 10 }

func ps(line string) ExecOpts { return ExecOpts{Cwd: `C:\`, Line: line} }

func (windowsGuest) prepTarget(dst string) ExecOpts {
	return ps(fmt.Sprintf(`$p=%s; if (Test-Path -LiteralPath $p -PathType Container) { 'dir' } else { New-Item -ItemType Directory -Force -Path (Split-Path -Parent $p) | Out-Null; 'file' }`, psQuote(dst)))
}

func (windowsGuest) packIfDir(src, tar string) ExecOpts {
	return ps(fmt.Sprintf(`$p=%[1]s; if (Test-Path -LiteralPath $p -PathType Container) { tar.exe -cf %[2]s -C $p .; if ($LASTEXITCODE) { exit $LASTEXITCODE }; 'dir' } elseif (Test-Path -LiteralPath $p) { 'file' } else { [Console]::Error.WriteLine("no such file or directory: $p"); exit 2 }`,
		psQuote(src), psQuote(tar)))
}

func (windowsGuest) unpack(tar, dst string) ExecOpts {
	return ps(fmt.Sprintf(`New-Item -ItemType Directory -Force -Path %[2]s | Out-Null; tar.exe -xf %[1]s -C %[2]s`, psQuote(tar), psQuote(dst)))
}

func (windowsGuest) remove(p string) ExecOpts {
	return ps("Remove-Item -Force -LiteralPath " + psQuote(p))
}

func (windowsGuest) gitClone(url, ref, dest string) ExecOpts {
	argv := []string{"git", "clone", "-q", "--depth", "1", "--", url, dest}
	if ref != "" {
		argv = []string{"git", "clone", "-q", "--branch", ref, "--depth", "1", "--", url, dest}
	}
	return ExecOpts{Cwd: `C:\`, Argv: argv}
}

// gui runs o as a scheduled task: programs the guest agent starts live in
// the invisible service session, and the task puts them on Administrator's
// desktop.
func (g windowsGuest) gui(o ExecOpts) ExecOpts {
	u := utf16.Encode([]rune(psScript(cwdOf(g, o), o)))
	b := make([]byte, len(u)*2)
	for i, c := range u {
		b[2*i], b[2*i+1] = byte(c), byte(c>>8)
	}
	enc := base64.StdEncoding.EncodeToString(b)
	return ps(`$n = 'veems-gui-' + [guid]::NewGuid()
$a = New-ScheduledTaskAction -Execute 'powershell.exe' -Argument '-NoProfile -WindowStyle Hidden -ExecutionPolicy Bypass -EncodedCommand ` + enc + `'
$p = New-ScheduledTaskPrincipal -UserId 'Administrator' -LogonType Interactive -RunLevel Highest
Register-ScheduledTask -TaskName $n -Action $a -Principal $p -Settings (New-ScheduledTaskSettingsSet -ExecutionTimeLimit 0 -AllowStartIfOnBatteries) | Out-Null
Start-ScheduledTask -TaskName $n`)
}

// PowerShell's start-up is most of every command's time, so run it once and
// let clones find its files in the page cache.
func (g windowsGuest) beforeSnapshot(a *agentConn) {
	for i := 0; i < 2; i++ {
		if p, err := g.proc(a, ExecOpts{Line: `Get-Date | Out-Null; Get-ChildItem C:\ | Out-Null`}); err == nil {
			a.run(p, 30*time.Second)
		}
	}
}
