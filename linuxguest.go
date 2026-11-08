package main

import (
	"fmt"
	"os"
	"path"
	"strings"
	"time"
)

type linuxGuest struct{ desktop bool }

const linuxPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

func (linuxGuest) workDir() string { return "/work" }

// execWrapper runs a command in its own session with its output going to
// files, then prints the files. The agent's own pipes would only close once
// every process holding them exited, so a job the command leaves running
// (`server &`) would hang the exec until it timed out; the shell instead
// waits for the command alone. Tools go by full path because o.Env may set
// PATH. killTree relies on the layout: the command is the wrapper's child
// and leads its own session.
var execWrapper = fmt.Sprintf(`cd -- "$1" || exit %[1]d
shift
o=/var/tmp/.veems-$$.out e=/var/tmp/.veems-$$.err
: >"$o" && : >"$e" || exit %[1]d
/usr/bin/setsid -w "$@" >"$o" 2>"$e"
c=$?
/bin/cat "$o"
/bin/cat "$e" >&2
/bin/rm -f "$o" "$e"
exit $c`, exitVeems)

// argv is the program and arguments o runs; a shell line goes to bash with
// shellFlags.
func argv(o ExecOpts, shellFlags string) []string {
	if o.Line != "" {
		return []string{"/bin/bash", shellFlags, o.Line}
	}
	return o.Argv
}

func (g linuxGuest) proc(_ *agentConn, o ExecOpts) (guestProc, error) {
	env := []string{"HOME=/root", "USER=root", "PATH=" + linuxPath, "LANG=C.UTF-8", "TERM=dumb"}
	if g.desktop { // so xdotool, wmctrl, and friends reach the desktop
		env = append(env, "DISPLAY=:0", "XAUTHORITY=/home/agent/.Xauthority")
	}
	return guestProc{Path: "/bin/sh", Env: append(env, o.Env...),
		Args: append([]string{"-c", execWrapper, "veems", cwdOf(g, o)}, argv(o, "-c")...)}, nil
}

func (linuxGuest) killTree(pid int) guestProc {
	return guestProc{Path: "/bin/sh", Args: []string{"-c",
		`for c in $(pgrep -P "$1"); do pkill -9 -s "$c"; kill -9 "$c"; done`, "veems", fmt.Sprint(pid)}}
}

func (g linuxGuest) abs(p string) string {
	if strings.HasPrefix(p, "/") {
		return p
	}
	return path.Join(g.workDir(), p)
}

func (linuxGuest) base(p string) string         { return path.Base(p) }
func (linuxGuest) join(dir, name string) string { return path.Join(dir, name) }
func (linuxGuest) temp(ext string) string       { return tempName("/var/tmp", "/", ext) } // /tmp is in RAM
func (linuxGuest) readChunk() int               { return 4 << 20 }

func sh(script string, args ...string) ExecOpts {
	return ExecOpts{Cwd: "/", Argv: append([]string{"/bin/sh", "-c", script, "veems"}, args...)}
}

func (linuxGuest) prepTarget(dst, name string, mode os.FileMode) ExecOpts {
	return sh(`t=$1; if [ -d "$t" ]; then t=$t/$2; else mkdir -p -- "$(dirname -- "$t")" || exit 1; fi
[ -d "$t" ] && { echo "$t is a directory" >&2; exit 1; }
: >"$t" && chmod "$3" "$t" && printf '%s' "$t"`, dst, name, fmt.Sprintf("%o", mode.Perm()))
}

func (linuxGuest) packIfDir(src, tar string) ExecOpts {
	return sh(`if [ -d "$1" ]; then tar -cf "$2" -C "$1" . && echo dir
elif [ -f "$1" ]; then echo "file $(stat -c %a -- "$1")"
elif [ -e "$1" ]; then echo "not a regular file or directory: $1" >&2; exit 2
else echo "no such file or directory: $1" >&2; exit 2; fi`, src, tar)
}

func (linuxGuest) unpack(tar, dst string) ExecOpts {
	return sh(`mkdir -p -- "$2" && tar --no-same-owner -xf "$1" -C "$2"`, tar, dst)
}

func (linuxGuest) remove(p string) ExecOpts { return sh(`rm -f -- "$1"`, p) }

func (linuxGuest) gitClone(url, ref, dest string) ExecOpts {
	return sh(`set -e
if [ -z "$2" ]; then exec git clone -q --depth 1 -- "$1" "$3"; fi
git init -q "$3"; cd "$3"; git remote add origin "$1"
git fetch -q --depth 1 origin "$2"; git checkout -q FETCH_HEAD`, url, ref, dest)
}

// gui starts o as the desktop user, in a login shell for a shell line so
// it gets that user's desktop environment.
func (g linuxGuest) gui(o ExecOpts) ExecOpts {
	r := sh(fmt.Sprintf(`cd -- "$1" || exit %d; shift
u=$(/usr/bin/id -u agent)
/usr/bin/setsid -f /usr/sbin/runuser -u agent -- /usr/bin/env DISPLAY=:0 XAUTHORITY=/home/agent/.Xauthority HOME=/home/agent USER=agent \
  DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$u/bus XDG_RUNTIME_DIR=/run/user/$u "$@" >/dev/null 2>&1 </dev/null`, exitVeems),
		append([]string{cwdOf(g, o)}, argv(o, "-lc")...)...)
	r.Env = o.Env
	return r
}

// beforeSnapshot drops the page cache, which keeps the snapshot (and every
// restore) small, then runs a command so the files every exec needs are the
// ones left in it.
func (g linuxGuest) beforeSnapshot(a *agentConn) {
	a.run(guestProc{Path: "/bin/sh", Args: []string{"-c", "sync; echo 3 > /proc/sys/vm/drop_caches"}}, 10*time.Second)
	for i := 0; i < 2; i++ {
		if p, err := g.proc(a, ExecOpts{Line: "true"}); err == nil {
			a.run(p, 10*time.Second)
		}
	}
}
