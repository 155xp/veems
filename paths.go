package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// Profile describes a guest OS: its defaults and where its images live.
type Profile struct {
	OS      string // "linux" or "windows"
	Prefix  string // id prefix
	CPUs    int
	MemMB   int
	WorkDir string // default cwd and default copy target inside the guest
	DiskGB  int
	Boot    int // seconds to wait for a cold boot
}

var profiles = map[string]*Profile{
	"linux":   {OS: "linux", Prefix: "lnx", CPUs: 2, MemMB: 1024, WorkDir: "/work", DiskGB: 16, Boot: 180},
	"windows": {OS: "windows", Prefix: "win", CPUs: 2, MemMB: 2048, WorkDir: `C:\work`, DiskGB: 40, Boot: 600},
}

func profile(name string) (*Profile, error) {
	switch name {
	case "lin", "lnx", "debian":
		name = "linux"
	case "win", "windows-server":
		name = "windows"
	}
	p, ok := profiles[name]
	if !ok {
		return nil, fmt.Errorf("unknown os %q (use linux or windows)", name)
	}
	return p, nil
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

func cacheDir() string          { return filepath.Join(veemsHome(), "cache") }
func vmsDir() string            { return filepath.Join(veemsHome(), "vms") }
func imageDir(os string) string { return filepath.Join(veemsHome(), "images", os) }
func vmDir(id string) string    { return filepath.Join(vmsDir(), id) }

// Image files for one OS.
func basePath(os string) string  { return filepath.Join(imageDir(os), "base.qcow2") }
func tmplDisk(os string) string  { return filepath.Join(imageDir(os), "snap.qcow2") }
func tmplState(os string) string { return filepath.Join(imageDir(os), "snap.state") }
func tmplMeta(os string) string  { return filepath.Join(imageDir(os), "snap.json") }

func exists(p string) bool { _, err := os.Stat(p); return err == nil }
