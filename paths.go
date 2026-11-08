package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Profile describes one image variant: its defaults and where it lives.
type Profile struct {
	Key     string // image name: linux, windows, linux-desktop, windows-desktop
	OS      string // "linux" or "windows"
	Desktop bool   // graphical desktop with auto-login
	Prefix  string // id prefix
	CPUs    int
	MemMB   int
	DiskGB  int
	Boot    time.Duration // longest a cold boot may take
	// How long a freshly booted guest is left alone before its snapshot, so
	// first-boot work (Windows' .NET and services, a desktop logging in) is
	// done and clones start idle.
	Settle time.Duration
}

var profiles = map[string]*Profile{
	"linux":           {Key: "linux", OS: "linux", Prefix: "lnx", CPUs: 2, MemMB: 1024, DiskGB: 16, Boot: 3 * time.Minute, Settle: 5 * time.Second},
	"windows":         {Key: "windows", OS: "windows", Prefix: "win", CPUs: 2, MemMB: 2048, DiskGB: 40, Boot: 10 * time.Minute, Settle: 3 * time.Minute},
	"linux-desktop":   {Key: "linux-desktop", OS: "linux", Desktop: true, Prefix: "lnxd", CPUs: 2, MemMB: 2048, DiskGB: 20, Boot: 4 * time.Minute, Settle: 40 * time.Second},
	"windows-desktop": {Key: "windows-desktop", OS: "windows", Desktop: true, Prefix: "wind", CPUs: 2, MemMB: 4096, DiskGB: 48, Boot: 15 * time.Minute, Settle: 3 * time.Minute},
}

func profile(name string, desktop bool) (*Profile, error) {
	name, d := strings.CutSuffix(name, "-desktop")
	switch name {
	case "lin", "lnx", "debian":
		name = "linux"
	case "win", "windows-server":
		name = "windows"
	}
	if d || desktop {
		name += "-desktop"
	}
	p, ok := profiles[name]
	if !ok {
		return nil, fmt.Errorf("unknown os %q (use linux or windows, optionally with --desktop)", name)
	}
	return p, nil
}

func (p *Profile) buildCmd() string {
	if p.Desktop {
		return "veems build " + p.OS + " --desktop"
	}
	return "veems build " + p.OS
}

func metaProfile(m *Meta) *Profile {
	p, _ := profile(m.OS, m.Desktop)
	return p
}

func veemsHome() string {
	if v := os.Getenv("VEEMS_HOME"); v != "" {
		return v
	}
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return filepath.Join(v, "veems")
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".local", "share", "veems")
}

func cacheDir() string           { return filepath.Join(veemsHome(), "cache") }
func vmsDir() string             { return filepath.Join(veemsHome(), "vms") }
func imageDir(key string) string { return filepath.Join(veemsHome(), "images", key) }
func vmDir(id string) string     { return filepath.Join(vmsDir(), id) }

func basePath(key string) string  { return filepath.Join(imageDir(key), "base.qcow2") }
func tmplDisk(key string) string  { return filepath.Join(imageDir(key), "snap.qcow2") }
func tmplState(key string) string { return filepath.Join(imageDir(key), "snap.state") }
func tmplMeta(key string) string  { return filepath.Join(imageDir(key), "snap.json") }

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// writeAtomic replaces path with data, so readers see the old or the new
// file, never part of one.
func writeAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// lockFile takes an exclusive lock on path, waiting for it or failing at
// once. Closing the file releases it.
func lockFile(path string, wait bool) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	how := syscall.LOCK_EX
	if !wait {
		how |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// readQEMUFile reads a file QEMU wrote. A compromised QEMU could have put a
// symlink, a FIFO, or a huge file in its place.
func readQEMUFile(path string, max int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() || st.Size() > max {
		return nil, fmt.Errorf("%s: not a regular file of at most %d bytes", path, max)
	}
	return io.ReadAll(io.LimitReader(f, max))
}

const oPath = 0x200000 // O_PATH on x86-64 (all veems runs on); package syscall leaves it out

// dialQEMU connects to a socket QEMU made, refusing anything else in its
// place. The socket is pinned by an O_PATH descriptor and reached through
// /proc, so it cannot be swapped for a symlink between the check and connect.
func dialQEMU(path string, timeout time.Duration) (net.Conn, error) {
	fd, err := syscall.Open(path, oPath|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	defer syscall.Close(fd)
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil || st.Mode&syscall.S_IFMT != syscall.S_IFSOCK {
		return nil, fmt.Errorf("%s: not a socket", path)
	}
	c, err := net.DialTimeout("unix", fmt.Sprintf("/proc/self/fd/%d", fd), timeout)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// checkRegular refuses anything but a regular file where QEMU was to write one.
func checkRegular(path string) error {
	if st, err := os.Lstat(path); err != nil || !st.Mode().IsRegular() {
		return fmt.Errorf("%s: not a regular file", path)
	}
	return nil
}

func lastLine(path string) string {
	b, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
