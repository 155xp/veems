package main

import (
	"fmt"
	"path"
	"strings"
	"time"
)

type linuxGuest struct{ desktop bool }

const linuxPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

func (linuxGuest) workDir() string { return "/work" }

// execWrapper runs a command in its own session with its output going to
// files, waits for that command only, then prints the files. Output through
// the agent's pipes would only end once every process holding them exits, so
// a job the command leaves running (`server &`) would hang the exec until it
// timed out. The session lets a timeout kill everything the command started.
const execWrapper = `cd -- "$1" || exit 125
shift
o=/tmp/.veems-$$.out e=/tmp/.veems-$$.err
exec 3<&0
setsid "$@" <&3 3<&- >"$o" 2>"$e" &
wait $! 2>/dev/null
c=$?
cat "$o"
cat "$e" >&2
rm -f "$o" "$e"
exit $c`

func (g linuxGuest) proc(_ *agentConn, o ExecOpts) (guestProc, error) {
	env := []string{"HOME=/root", "USER=root", "PATH=" + linuxPath, "LANG=C.UTF-8", "TERM=dumb"}
	if g.desktop { // so xdotool, wmctrl, and friends reach the desktop
		env = append(env, "DISPLAY=:0", "XAUTHORITY=/home/agent/.Xauthority")
	}
	cmd := o.Argv
	if o.Line != "" {
		cmd = []string{"/bin/bash", "-c", o.Line}
	}
	return guestProc{Path: "/bin/sh", Env: append(env, o.Env...),
		Args: append([]string{"-c", execWrapper, "veems", cwdOf(g, o)}, cmd...)}, nil
}

// killTree kills the session of every child of pid: with execWrapper, that
// is the command and everything it started.
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
func (linuxGuest) temp(ext string) string       { return tempName("/tmp", "/", ext) }
func (linuxGuest) readChunk() int               { return 4 << 20 }

func sh(script string, args ...string) ExecOpts {
	return ExecOpts{Cwd: "/", Argv: append([]string{"/bin/sh", "-c", script, "veems"}, args...)}
}

func (linuxGuest) prepTarget(dst string) ExecOpts {
	return sh(`if [ -d "$1" ]; then echo dir; else mkdir -p -- "$(dirname -- "$1")" && echo file; fi`, dst)
}

func (linuxGuest) packIfDir(src, tar string) ExecOpts {
	return sh(`if [ -d "$1" ]; then tar -cf "$2" -C "$1" . && echo dir
elif [ -f "$1" ]; then echo file
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

func (g linuxGuest) gui(o ExecOpts) ExecOpts {
	cmd := o.Argv
	if o.Line != "" {
		cmd = []string{"/bin/bash", "-lc", o.Line}
	}
	r := sh(`cd -- "$1" || exit 125; shift
u=$(id -u agent)
setsid -f runuser -u agent -- env DISPLAY=:0 XAUTHORITY=/home/agent/.Xauthority HOME=/home/agent USER=agent \
  DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$u/bus XDG_RUNTIME_DIR=/run/user/$u "$@" >/dev/null 2>&1 </dev/null`,
		append([]string{cwdOf(g, o)}, cmd...)...)
	r.Env = o.Env
	return r
}

// Dropping the page cache keeps the snapshot, and so every restore, small.
func (linuxGuest) beforeSnapshot(a *agentConn) {
	a.run(guestProc{Path: "/bin/sh", Args: []string{"-c", "sync; echo 3 > /proc/sys/vm/drop_caches"}}, 10*time.Second)
}
