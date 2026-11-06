package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

// A project is a snapshot of a VM that already holds a checkout and has run
// its setup, so `veems new --project NAME` starts with the work done.
type Project struct {
	Name     string    `json:"name"`
	Image    string    `json:"image"` // profile key it was built on: linux, windows, ...
	Git      string    `json:"git,omitempty"`
	Ref      string    `json:"ref,omitempty"`
	Src      string    `json:"src,omitempty"` // host directory copied in
	Dest     string    `json:"dest"`          // where the checkout lives in the guest
	Setup    string    `json:"setup,omitempty"`
	Net      bool      `json:"net"` // the network was on while preparing
	Timeout  string    `json:"timeout,omitempty"`
	Base     string    `json:"base"` // stamp of the image snapshot it was built on
	QEMU     string    `json:"qemu"`
	CPUs     int       `json:"cpus"`
	MemMB    int       `json:"mem_mb"`
	Prepared time.Time `json:"prepared"`
	PrepSecs float64   `json:"prep_secs"`
}

var projectName = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,39}$`)

func projectsDir() string           { return filepath.Join(veemsHome(), "projects") }
func projectDir(name string) string { return filepath.Join(projectsDir(), name) }
func projDisk(name string) string   { return filepath.Join(projectDir(name), "disk.qcow2") }
func projState(name string) string  { return filepath.Join(projectDir(name), "snap.state") }
func projMeta(name string) string   { return filepath.Join(projectDir(name), "project.json") }

func checkProjectName(name string) error {
	if !projectName.MatchString(name) || strings.Contains(name, "..") {
		return fmt.Errorf("bad project name %q: use lowercase letters, digits, '.', '_' and '-'", name)
	}
	return nil
}

func loadProject(name string) (*Project, error) {
	if err := checkProjectName(name); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(projMeta(name))
	if err != nil || !exists(projDisk(name)) || !exists(projState(name)) {
		return nil, fmt.Errorf("no project %q (see `veems project ls`)", name)
	}
	var p Project
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("project %s: %w", name, err)
	}
	return &p, nil
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

// stale says why a project can no longer be used as is, or "".
func (p *Project) stale() string {
	switch snap := readSnap(p.Image); {
	case snap == nil:
		return fmt.Sprintf("the %s image has no snapshot", p.Image)
	case stamp(tmplState(p.Image)) != p.Base:
		return fmt.Sprintf("the %s image was rebuilt", p.Image)
	case p.QEMU != qemuVersion():
		return "QEMU was upgraded"
	}
	return ""
}

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
	return &source{prof: p, project: name, disk: projDisk(name), state: projState(name),
		snap:  &snapInfo{QEMU: pj.QEMU, CPUs: pj.CPUs, MemMB: pj.MemMB},
		binds: []string{imageDir(p.Key), projectDir(name)}}, nil
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
		flag := ""
		if p.Desktop {
			flag = " --desktop"
		}
		return nil, fmt.Errorf("no %s image yet: run `veems build %s%s` once", p.Key, p.OS, flag)
	}
	return imageSource(p), nil
}

// metaSource is the source a running VM was created from.
func metaSource(m *Meta) *source {
	p := metaProfile(m)
	if m.Project != "" {
		return &source{prof: p, project: m.Project, disk: projDisk(m.Project), state: projState(m.Project),
			binds: []string{imageDir(p.Key), projectDir(m.Project)}}
	}
	return &source{prof: p, disk: tmplDisk(p.Key), state: tmplState(p.Key), binds: []string{imageDir(p.Key)}}
}

type PrepOpts struct {
	Name    string
	OS      string
	Desktop bool
	Git     string
	Ref     string
	Src     string
	Dest    string
	Setup   string
	Net     bool
	Timeout time.Duration
}

// prepare builds a project: start a VM from the image snapshot, put the code
// in, run setup, then save the running VM (RAM and disk) as the project's
// snapshot. The network, if setup needed it, is unplugged before the save, so
// VMs restored from the project get the network only when they ask for it.
// The VM is saved with its agent connection open (through the supervisor's
// proxy), which is the state clones resume into.
func prepare(o PrepOpts) (*Project, error) {
	if err := checkProjectName(o.Name); err != nil {
		return nil, err
	}
	p, err := profile(o.OS, o.Desktop)
	if err != nil {
		return nil, err
	}
	snap := readSnap(p.Key)
	if snap == nil || snap.QEMU != qemuVersion() {
		return nil, fmt.Errorf("the %s image needs a current snapshot first: `veems build %s --snapshot`", p.Key, p.OS)
	}
	if err := os.MkdirAll(projectsDir(), 0o755); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(projectsDir(), "."+o.Name+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return nil, fmt.Errorf("project %s is already being prepared", o.Name)
	}
	if o.Src != "" {
		if o.Src, err = filepath.Abs(o.Src); err != nil {
			return nil, err
		}
		if st, err := os.Stat(o.Src); err != nil || !st.IsDir() {
			return nil, fmt.Errorf("--src %s: not a directory", o.Src)
		}
	}
	if o.Dest == "" {
		o.Dest = defaultDest(p, o)
	}
	pj := &Project{Name: o.Name, Image: p.Key, Git: o.Git, Ref: o.Ref, Src: o.Src, Dest: o.Dest, Setup: o.Setup,
		Net: o.Net || o.Git != "", Base: stamp(tmplState(p.Key)), QEMU: snap.QEMU, CPUs: snap.CPUs, MemMB: snap.MemMB}
	if o.Timeout > 0 {
		pj.Timeout = o.Timeout.String()
	}
	start := time.Now()

	m, err := createVM(NewOpts{OS: p.OS, Desktop: p.Desktop, Name: "prep-" + o.Name, Net: pj.Net, NetHub: true, Quiet: true, NoPool: true})
	if err != nil {
		return nil, err
	}
	defer destroy(m.ID)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sig)
	go func() {
		if _, ok := <-sig; ok {
			destroy(m.ID)
			os.Exit(130)
		}
	}()
	logf("preparing %s in %s", o.Name, m.ID)

	step := func(what string, eo ExecOpts) error {
		logf("  %s", what)
		if eo.Timeout == 0 {
			eo.Timeout = o.Timeout
		}
		r, err := guestExec(m, eo)
		if err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		if r.ExitCode != 0 {
			os.Stderr.Write(r.Stdout)
			os.Stderr.Write(r.Stderr)
			if r.TimedOut {
				return fmt.Errorf("%s: timed out after %s (raise --timeout)", what, o.Timeout)
			}
			return fmt.Errorf("%s: exit code %d", what, r.ExitCode)
		}
		return nil
	}
	if o.Git != "" {
		var err error
		if p.OS == "windows" {
			err = step("git clone "+o.Git, ExecOpts{Cwd: `C:\`, Argv: gitArgs("git", o.Git, o.Ref, o.Dest)})
		} else {
			err = step("git clone "+o.Git, ExecOpts{Cwd: "/", Argv: []string{"/bin/sh", "-c", gitScript, "veems", o.Git, o.Ref, o.Dest}})
		}
		if err != nil {
			return nil, err
		}
	}
	if o.Src != "" {
		logf("  copying %s", o.Src)
		if err := push(m, o.Src, o.Dest); err != nil {
			return nil, fmt.Errorf("copy %s: %w", o.Src, err)
		}
	}
	if o.Setup != "" {
		cwd := o.Dest
		if o.Git == "" && o.Src == "" {
			cwd = ""
		}
		if err := step("setup: "+o.Setup, ExecOpts{Line: o.Setup, Cwd: cwd}); err != nil {
			return nil, err
		}
	}
	// The page cache stays in the snapshot: freed pages would be saved all
	// the same, and this way the first command in a clone finds its files warm.
	dir := vmDir(m.ID)
	if pj.Net {
		for _, id := range []string{"uplinkport", "uplink"} {
			if _, err := qmp(dir, "netdev_del", map[string]any{"id": id}, 10*time.Second); err != nil {
				return nil, fmt.Errorf("unplugging the network: %w", err)
			}
		}
	}
	if err := saveState(dir, filepath.Join(dir, "snap.state")); err != nil {
		return nil, err
	}

	// Assemble the new project next to the old one, then swap it in.
	work, err := os.MkdirTemp(projectsDir(), "."+o.Name+"-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	if err := os.Rename(filepath.Join(dir, "disk.qcow2"), filepath.Join(work, "disk.qcow2")); err != nil {
		return nil, err
	}
	if err := os.Rename(filepath.Join(dir, "snap.state"), filepath.Join(work, "snap.state")); err != nil {
		return nil, err
	}
	os.Chmod(filepath.Join(work, "disk.qcow2"), 0o444)
	os.Chmod(filepath.Join(work, "snap.state"), 0o444)
	pj.Prepared = time.Now().UTC()
	pj.PrepSecs = time.Since(start).Seconds()
	b, _ := json.MarshalIndent(pj, "", "  ")
	if err := os.WriteFile(filepath.Join(work, "project.json"), b, 0o644); err != nil {
		return nil, err
	}
	os.Chmod(work, 0o755)
	old := work + ".old"
	if exists(projectDir(o.Name)) {
		if err := os.Rename(projectDir(o.Name), old); err != nil {
			return nil, err
		}
		defer os.RemoveAll(old) // VMs still running from it keep their open files
	}
	if err := os.Rename(work, projectDir(o.Name)); err != nil {
		return nil, err
	}
	st, _ := os.Stat(projState(o.Name))
	logf("project %s ready in %s (%d MB state)", o.Name, time.Since(start).Round(time.Second), st.Size()>>20)
	return pj, nil
}

// gitScript clones $1 at ref $2 (default branch if empty) into $3, shallow.
const gitScript = `set -e
if [ -z "$2" ]; then exec git clone -q --depth 1 -- "$1" "$3"; fi
git init -q "$3"; cd "$3"; git remote add origin "$1"
git fetch -q --depth 1 origin "$2"; git checkout -q FETCH_HEAD`

func gitArgs(git, url, ref, dest string) []string {
	if ref == "" {
		return []string{git, "clone", "-q", "--depth", "1", "--", url, dest}
	}
	return []string{git, "clone", "-q", "--branch", ref, "--depth", "1", "--", url, dest}
}

func defaultDest(p *Profile, o PrepOpts) string {
	base := ""
	switch {
	case o.Git != "":
		base = strings.TrimSuffix(path.Base(strings.TrimRight(strings.ReplaceAll(o.Git, `\`, "/"), "/")), ".git")
	case o.Src != "":
		base = filepath.Base(o.Src)
	}
	if base == "" || base == "." || base == "/" || strings.ContainsAny(base, `:`) {
		base = o.Name
	}
	if p.OS == "windows" {
		return p.WorkDir + `\` + base
	}
	return path.Join(p.WorkDir, base)
}

// saveState pauses a VM and writes its RAM and device state to file. Pausing
// flushes the disk, so the disk and the state match.
func saveState(dir, file string) error {
	if _, err := qmp(dir, "stop", nil, 30*time.Second); err != nil {
		return fmt.Errorf("pausing vm: %w", err)
	}
	if _, err := qmp(dir, "migrate", map[string]any{"uri": "file:" + file}, 30*time.Second); err != nil {
		return fmt.Errorf("saving vm state: %w", err)
	}
	for {
		raw, err := qmp(dir, "query-migrate", nil, 10*time.Second)
		if err != nil {
			return err
		}
		var st struct {
			Status string `json:"status"`
			Error  string `json:"error-desc"`
		}
		json.Unmarshal(raw, &st)
		switch st.Status {
		case "completed":
			return nil
		case "failed", "cancelled":
			return fmt.Errorf("saving vm state: %s %s", st.Status, st.Error)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func listProjects() []*Project {
	ents, _ := os.ReadDir(projectsDir())
	var out []*Project
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if pj, err := loadProject(e.Name()); err == nil {
			out = append(out, pj)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (pj *Project) prepOpts() (PrepOpts, error) {
	p := profiles[pj.Image]
	if p == nil {
		return PrepOpts{}, fmt.Errorf("project %s: unknown image %q", pj.Name, pj.Image)
	}
	var t time.Duration
	if pj.Timeout != "" {
		t, _ = time.ParseDuration(pj.Timeout)
	}
	return PrepOpts{Name: pj.Name, OS: p.OS, Desktop: p.Desktop, Git: pj.Git, Ref: pj.Ref, Src: pj.Src,
		Dest: pj.Dest, Setup: pj.Setup, Net: pj.Net, Timeout: t}, nil
}

func removeProject(name string) error {
	if _, err := loadProject(name); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(projectsDir(), "."+name+"-rm-")
	if err != nil {
		return err
	}
	os.Remove(tmp)
	if err := os.Rename(projectDir(name), tmp); err != nil {
		return err
	}
	return os.RemoveAll(tmp)
}
