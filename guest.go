package main

import (
	"fmt"
	"os"
	"time"
)

// guestOS is everything that differs between the Linux and Windows guests:
// how a command is started, what paths look like, and the small helper
// commands that copies, projects, and the desktop run inside the guest.
type guestOS interface {
	workDir() string // default cwd, and where relative copy paths land
	// proc turns o into the process the guest agent starts. It may write a
	// file through a first.
	proc(a *agentConn, o ExecOpts) (guestProc, error)
	killTree(pid int) guestProc

	abs(p string) string // relative paths are under workDir
	base(p string) string
	join(dir, name string) string
	temp(ext string) string
	readChunk() int // bytes per guest-file-read

	// prepTarget creates the file a copy of name to dst writes (inside dst
	// if that is a directory) with mode, and prints its path.
	prepTarget(dst, name string, mode os.FileMode) ExecOpts
	packIfDir(src, tar string) ExecOpts // prints "dir" after tarring src into tar, or "file" and maybe its octal mode; fails if src is missing
	unpack(tar, dst string) ExecOpts
	remove(p string) ExecOpts
	gitClone(url, ref, dest string) ExecOpts
	gui(o ExecOpts) ExecOpts // starts o on the desktop and returns at once

	// beforeSnapshot readies an idle guest for a snapshot every VM will
	// restore from.
	beforeSnapshot(a *agentConn)
}

// guestProc is one process as the guest agent's guest-exec takes it.
type guestProc struct {
	Path  string
	Args  []string
	Env   []string
	Input []byte
}

func guestFor(osName string, desktop bool) guestOS {
	if osName == "windows" {
		return windowsGuest{}
	}
	return linuxGuest{desktop: desktop}
}

func (m *Meta) guest() guestOS    { return guestFor(m.OS, m.Desktop) }
func (p *Profile) guest() guestOS { return guestFor(p.OS, p.Desktop) }

func cwdOf(g guestOS, o ExecOpts) string {
	if o.Cwd != "" {
		return o.Cwd
	}
	return g.workDir()
}

func tempName(dir, sep, ext string) string {
	return fmt.Sprintf("%s%sveems-%d%s", dir, sep, time.Now().UnixNano(), ext)
}
