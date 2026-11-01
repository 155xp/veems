package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Profile describes one image variant: its defaults and where it lives.
type Profile struct {
	Key     string // image name: linux, windows, linux-desktop, windows-desktop
	OS      string // "linux" or "windows"
	Desktop bool   // graphical desktop with auto-login
	Prefix  string // id prefix
	CPUs    int
	MemMB   int
	WorkDir string // default cwd and default copy target inside the guest
	DiskGB  int
	Boot    int // seconds to wait for a cold boot
}

var profiles = map[string]*Profile{
	"linux":           {Key: "linux", OS: "linux", Prefix: "lnx", CPUs: 2, MemMB: 1024, WorkDir: "/work", DiskGB: 16, Boot: 180},
	"windows":         {Key: "windows", OS: "windows", Prefix: "win", CPUs: 2, MemMB: 2048, WorkDir: `C:\work`, DiskGB: 40, Boot: 600},
	"linux-desktop":   {Key: "linux-desktop", OS: "linux", Desktop: true, Prefix: "lnxd", CPUs: 2, MemMB: 2048, WorkDir: "/work", DiskGB: 20, Boot: 240},
	"windows-desktop": {Key: "windows-desktop", OS: "windows", Desktop: true, Prefix: "wind", CPUs: 2, MemMB: 4096, WorkDir: `C:\work`, DiskGB: 48, Boot: 900},
}

// profile resolves an OS name, plus --desktop, to an image variant.
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

// Image files for one variant.
func basePath(key string) string  { return filepath.Join(imageDir(key), "base.qcow2") }
func tmplDisk(key string) string  { return filepath.Join(imageDir(key), "snap.qcow2") }
func tmplState(key string) string { return filepath.Join(imageDir(key), "snap.state") }
func tmplMeta(key string) string  { return filepath.Join(imageDir(key), "snap.json") }

func exists(p string) bool { _, err := os.Stat(p); return err == nil }
