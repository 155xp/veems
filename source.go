package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// source is what a new VM starts from: an image's snapshot or a project's.
type source struct {
	prof    *Profile
	project string
	disk    string    // backing disk for the VM's copy-on-write layer
	state   string    // saved RAM and device state
	snap    *snapInfo // nil: no usable state, cold boot from disk
	binds   []string  // read-only directories QEMU needs
}

func (s *source) stamp() string {
	if s.snap == nil {
		return stamp(s.disk)
	}
	return stamp(s.state)
}

func imageSource(p *Profile) *source {
	s := &source{prof: p, disk: basePath(p.Key), binds: []string{imageDir(p.Key)}}
	if snap := readSnap(p.Key); snap != nil {
		s.disk, s.state, s.snap = tmplDisk(p.Key), tmplState(p.Key), snap
	}
	return s
}

func projectSource(name string) (*source, error) {
	pj, err := loadProject(name)
	if err != nil {
		return nil, err
	}
	if why := pj.stale(); why != "" {
		return nil, fmt.Errorf("project %s is out of date (%s): run `veems project refresh %s`", name, why, name)
	}
	p := profiles[pj.Image]
	if p == nil {
		return nil, fmt.Errorf("project %s: unknown image %q", name, pj.Image)
	}
	s := projectFiles(p, name)
	s.snap = &snapInfo{QEMU: pj.QEMU, CPUs: pj.CPUs, MemMB: pj.MemMB}
	return s, nil
}

func projectFiles(p *Profile, name string) *source {
	return &source{prof: p, project: name, disk: projDisk(name), state: projState(name),
		binds: []string{imageDir(p.Key), projectDir(name)}}
}

// cold is s for a VM that boots instead of restoring. An image snapshot's
// disk only goes with its RAM state, so that VM boots from the base disk.
func (s *source) cold() *source {
	c := *s
	c.snap, c.state = nil, ""
	if c.project == "" {
		c.disk = basePath(c.prof.Key)
	}
	return &c
}

// sourceFor finds what o asks to start from.
func sourceFor(o NewOpts) (*source, error) {
	if o.Project != "" {
		s, err := projectSource(o.Project)
		if err != nil {
			return nil, err
		}
		if o.OS != "" {
			if p, err := profile(o.OS, o.Desktop); err != nil || p.Key != s.prof.Key {
				return nil, fmt.Errorf("project %s is a %s project", o.Project, s.prof.Key)
			}
		}
		return s, nil
	}
	p, err := profile(o.OS, o.Desktop)
	if err != nil {
		return nil, err
	}
	if !exists(basePath(p.Key)) {
		return nil, fmt.Errorf("no %s image yet: run `%s` once", p.Key, p.buildCmd())
	}
	return imageSource(p), nil
}

// metaSource is the source a running VM was created from.
func metaSource(m *Meta) *source {
	p := metaProfile(m)
	if m.Project != "" {
		return projectFiles(p, m.Project)
	}
	return imageSource(p)
}

type snapInfo struct {
	QEMU  string `json:"qemu"`
	CPUs  int    `json:"cpus"`
	MemMB int    `json:"mem_mb"`
}

func readSnap(key string) *snapInfo {
	b, err := os.ReadFile(tmplMeta(key))
	if err != nil || !exists(tmplState(key)) || !exists(tmplDisk(key)) {
		return nil
	}
	var s snapInfo
	if json.Unmarshal(b, &s) != nil {
		return nil
	}
	return &s
}

// current says whether this QEMU can restore the snapshot.
func (s *snapInfo) current() bool { return s != nil && s.QEMU == qemuVersion() }

// fits says whether a VM with cpus and memMB can restore the snapshot.
func (s *snapInfo) fits(cpus, memMB int) bool {
	return s.current() && s.CPUs == cpus && s.MemMB == memMB
}

// stamp identifies one saved snapshot. Rebuilding an image or re-preparing a
// project writes a new state file, so the stamp changes and anything made
// from the old one (projects, pooled VMs) is known to be stale.
func stamp(file string) string {
	st, err := os.Stat(file)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d.%d", st.Size(), st.ModTime().UnixNano())
}
