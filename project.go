package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
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
	src, err := sourceFor(NewOpts{OS: p.OS, Desktop: p.Desktop})
	if err != nil {
		return nil, err
	}
	snap := src.snap
	if !snap.current() {
		return nil, fmt.Errorf("the %s image needs a current snapshot first: `%s --snapshot`", p.Key, p.buildCmd())
	}
	if err := os.MkdirAll(projectsDir(), 0o700); err != nil {
		return nil, err
	}
	os.Chmod(projectsDir(), 0o700) // made 0755 by older versions
	lock, err := lockFile(filepath.Join(projectsDir(), "."+o.Name+".lock"), false)
	if err != nil {
		return nil, fmt.Errorf("project %s is already being prepared", o.Name)
	}
	defer lock.Close()
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

	// The idle limit only matters if this process dies: every exec counts as activity.
	m, err := startVM(src, NewOpts{Name: "prep-" + o.Name, Net: pj.Net, Idle: 20 * time.Minute})
	if err != nil {
		return nil, err
	}
	defer destroy(m.ID)
	defer onInterrupt(func() { destroy(m.ID) })()
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
		if err := step("git clone "+o.Git, p.guest().gitClone(o.Git, o.Ref, o.Dest)); err != nil {
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
	if err := saveState(dir, filepath.Join(runDir(dir), "snap.state")); err != nil {
		return nil, err
	}

	work, err := os.MkdirTemp(projectsDir(), "."+o.Name+"-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	if err := os.Rename(filepath.Join(dir, "disk.qcow2"), filepath.Join(work, "disk.qcow2")); err != nil {
		return nil, err
	}
	if err := os.Rename(filepath.Join(runDir(dir), "snap.state"), filepath.Join(work, "snap.state")); err != nil {
		return nil, err
	}
	destroy(m.ID) // QEMU holds the disk's write lock until it exits
	if err := checkRegular(filepath.Join(work, "snap.state")); err != nil {
		return nil, err
	}
	// The checkout and the saved RAM can hold private code and credentials.
	os.Chmod(filepath.Join(work, "disk.qcow2"), 0o400)
	os.Chmod(filepath.Join(work, "snap.state"), 0o400)
	pj.Prepared = time.Now().UTC()
	pj.PrepSecs = time.Since(start).Seconds()
	b, _ := json.MarshalIndent(pj, "", "  ")
	if err := os.WriteFile(filepath.Join(work, "project.json"), b, 0o600); err != nil {
		return nil, err
	}
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
	g := p.guest()
	return g.join(g.workDir(), base)
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
