package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Meta struct {
	ID       string    `json:"id"`
	Name     string    `json:"name,omitempty"`
	OS       string    `json:"os"`
	Desktop  bool      `json:"desktop,omitempty"`
	CPUs     int       `json:"cpus"`
	MemMB    int       `json:"mem_mb"`
	Net      bool      `json:"net"`
	Snapshot bool      `json:"from_snapshot"`
	Created  time.Time `json:"created"`
	Deadline time.Time `json:"deadline,omitzero"` // hard stop; zero = none
	IdleSecs int       `json:"idle_secs"`         // stop after this long with no veems activity; 0 = never
	PID      int       `json:"pid"`               // supervisor
	Project  string    `json:"project,omitempty"`
	Source   string    `json:"source,omitempty"` // stamp of the snapshot or disk it started from
	Pool     string    `json:"pool,omitempty"`   // waiting in this pool, not yet claimed
	Pooled   bool      `json:"-"`                // this process claimed it from a pool
}

// Besides its disk, sockets, and logs, a VM's directory holds marker files
// through which the processes that share the VM coordinate.
const (
	fileActivity = "activity" // touched by every veems command; the idle limit counts from its mtime
	fileReady    = "ready"    // the guest answered: the VM is up
	fileExited   = "exited"   // the supervisor quit before the VM was ready; qemu.log says why
	filePooled   = "pooled"   // waiting in a pool; whoever removes it has claimed the VM
)

func metaPath(id string) string { return filepath.Join(vmDir(id), "meta.json") }

func loadMeta(id string) (*Meta, error) {
	b, err := os.ReadFile(metaPath(id))
	if err != nil {
		return nil, fmt.Errorf("no such vm: %s", id)
	}
	var m Meta
	return &m, json.Unmarshal(b, &m)
}

func saveMeta(m *Meta) error {
	b, _ := json.MarshalIndent(m, "", "  ")
	return writeAtomic(metaPath(m.ID), b, 0o600)
}

func newID(prefix string) string {
	const al = "abcdefghjkmnpqrstuvwxyz23456789"
	b := make([]byte, 6)
	rand.Read(b)
	for i := range b {
		b[i] = al[int(b[i])%len(al)]
	}
	return prefix + "-" + string(b)
}

type NewOpts struct {
	OS      string
	Desktop bool
	Name    string
	CPUs    int
	MemMB   int
	Net     bool
	TTL     time.Duration
	Idle    time.Duration
	Cold    bool
	Project string // start from this project's snapshot
	NoPool  bool   // never claim a pooled VM
	Pool    string // start a VM for this pool
}

// newVM hands out a VM: a waiting one from a pool if one fits, else a fresh
// restore. Either way the pool is topped up afterwards.
func newVM(o NewOpts) (*Meta, error) {
	src, err := sourceFor(o)
	if err != nil {
		return nil, err
	}
	if o.Name != "" {
		vms, _ := listVMs()
		for _, m := range vms {
			if m.Name == o.Name && m.Pool == "" {
				return nil, fmt.Errorf("a vm named %s is already running (%s)", o.Name, m.ID)
			}
		}
	}
	key := poolKey(src.prof.Key, src.project, o.Net)
	defer refill(key)
	if !o.Cold && !o.NoPool {
		if m := claim(o, src, key); m != nil {
			return m, nil
		}
	}
	m, err := startVM(src, o)
	if errors.Is(err, errLowMemory) && len(pooled("")) > 0 {
		err = fmt.Errorf("%w; VMs waiting in a pool hold memory too (`veems pool` shows them)", err)
	}
	return m, err
}

func (m *Meta) limit(o NewOpts) {
	m.Name, m.IdleSecs = o.Name, int(o.Idle.Seconds())
	m.Created, m.Deadline = time.Now().UTC(), time.Time{}
	if o.TTL > 0 {
		m.Deadline = m.Created.Add(o.TTL)
	}
}

// wake checks that a guest answers and sets its clock: a restored snapshot
// wakes up with the clock it was saved with. One connection for both, since
// a new one would wait out the agent's reconnect pause.
func wake(id string, timeout time.Duration) error {
	a := guestSession(id)
	defer a.close()
	if err := a.call("guest-ping", nil, nil, timeout); err != nil {
		return err
	}
	a.call("guest-set-time", map[string]any{"time": time.Now().UnixNano()}, nil, 10*time.Second)
	return nil
}

// startVM restores (or, with no usable snapshot, boots) a new VM from src and
// waits until its guest agent answers.
func startVM(src *source, o NewOpts) (*Meta, error) {
	p := src.prof
	if o.CPUs == 0 {
		o.CPUs = p.CPUs
	}
	if o.MemMB == 0 {
		o.MemMB = p.MemMB
	}
	listVMs() // clears away VMs whose supervisor died, so their memory counts as free
	if err := checkHostMemory(o.MemMB); err != nil {
		return nil, err
	}
	snap := src.snap
	useSnap := !o.Cold && snap.fits(o.CPUs, o.MemMB)
	if !o.Cold && snap != nil && !useSnap {
		fix := "`" + p.buildCmd() + " --snapshot` refreshes it"
		if src.project != "" {
			fix = fmt.Sprintf("the project was saved with %d cpus and %d MB", snap.CPUs, snap.MemMB)
		}
		logf("snapshot does not match (cpus/mem/qemu version), cold booting; %s", fix)
	}
	if !useSnap {
		src = src.cold()
	}

	m := &Meta{ID: newID(p.Prefix), OS: p.OS, Desktop: p.Desktop, CPUs: o.CPUs, MemMB: o.MemMB, Net: o.Net,
		Snapshot: useSnap, Project: src.project, Pool: o.Pool, Source: src.stamp()}
	m.limit(o)
	dir := vmDir(m.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	fail := func(err error) (*Meta, error) { os.RemoveAll(dir); return nil, err }
	defer onInterrupt(func() { destroy(m.ID) })()
	if err := qemuImgCreate(filepath.Join(dir, "disk.qcow2"), src.disk, 0); err != nil {
		return fail(err)
	}
	os.WriteFile(filepath.Join(dir, fileActivity), nil, 0o600)
	if err := saveMeta(m); err != nil {
		return fail(err)
	}

	self, _ := os.Executable()
	sup := exec.Command(self, "_supervise", m.ID)
	sup.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := sup.Start(); err != nil {
		return fail(err)
	}
	go sup.Wait()

	wait := p.Boot
	if useSnap {
		wait = 60 * time.Second
	}
	deadline := time.Now().Add(wait)
	resumed := !useSnap
	var pausedAt time.Time
	for {
		if !exists(dir) || exists(filepath.Join(dir, fileExited)) {
			log, _ := os.ReadFile(filepath.Join(dir, "qemu.log"))
			os.RemoveAll(dir)
			return nil, fmt.Errorf("vm failed to start:\n%s", strings.TrimSpace(string(log)))
		}
		if !resumed {
			// QEMU restores the snapshot paused, as it was saved. Resume once
			// the supervisor's agent proxy is connected (see agentProxy), or
			// without it if the proxy never comes up.
			if raw, err := qmp(dir, "query-status", nil, 2*time.Second); err == nil {
				var st struct{ Status string }
				json.Unmarshal(raw, &st)
				if st.Status == "paused" && pausedAt.IsZero() {
					pausedAt = time.Now()
				}
				if st.Status == "paused" && (exists(filepath.Join(dir, "agent.sock")) || time.Since(pausedAt) > 2*time.Second) {
					if _, err := qmp(dir, "cont", nil, 5*time.Second); err == nil {
						resumed = true
					}
				} else if st.Status == "running" {
					resumed = true
				}
			}
		} else if wake(m.ID, 2*time.Second) == nil {
			break
		}
		if time.Now().After(deadline) {
			destroy(m.ID)
			return nil, fmt.Errorf("vm did not become ready within %s", wait)
		}
		if useSnap {
			time.Sleep(5 * time.Millisecond) // a restore takes a few hundred ms
		} else {
			time.Sleep(50 * time.Millisecond)
		}
	}
	os.WriteFile(filepath.Join(dir, fileReady), nil, 0o600)
	return m, nil
}

var errLowMemory = errors.New("not enough free memory")

// checkHostMemory refuses to start a VM that would push the host into swap.
func checkHostMemory(memMB int) error {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return nil
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "MemAvailable:"); ok {
			kb, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(v), " kB"))
			reserve := 1024
			if s := os.Getenv("VEEMS_RESERVE_MB"); s != "" {
				reserve, _ = strconv.Atoi(s)
			}
			if avail := kb / 1024; avail < memMB+reserve {
				return fmt.Errorf("%w: %d MB available, VM needs %d MB plus %d MB kept for the host (VEEMS_RESERVE_MB); remove idle VMs with `veems rm --all`", errLowMemory, avail, memMB, reserve)
			}
		}
	}
	return nil
}

// supervise owns one VM for its whole life: it starts QEMU, enforces the
// lifetime and idle limits, and deletes the VM directory when QEMU exits for
// any reason (shutdown inside the guest, `veems rm`, a crash, or a limit).
func supervise(id string) error {
	m, err := loadMeta(id)
	if err != nil {
		return err
	}
	dir := vmDir(id)
	m.PID = os.Getpid()
	saveMeta(m)
	syscall.Setpriority(syscall.PRIO_PROCESS, 0, 10) // the host comes first

	src := metaSource(m)
	s := Spec{OS: m.OS, Desktop: m.Desktop, Dir: dir, Disk: filepath.Join(dir, "disk.qcow2"), CPUs: m.CPUs, MemMB: m.MemMB,
		Net: m.Net, ROBinds: src.binds}
	if m.Snapshot {
		s.Incoming = src.state
	}
	cmd, err := launchCmd(s)
	if err == nil && m.Snapshot && stamp(src.state) != m.Source {
		err = fmt.Errorf("the snapshot changed while this vm was starting; try again")
	}
	if err != nil {
		os.WriteFile(filepath.Join(dir, "qemu.log"), []byte(err.Error()), 0o600)
		os.WriteFile(filepath.Join(dir, fileExited), nil, 0o600)
		return err
	}
	qlog, _ := os.Create(filepath.Join(dir, "qemu.log"))
	cmd.Stdout, cmd.Stderr = qlog, qlog
	if err := cmd.Start(); err != nil {
		os.WriteFile(filepath.Join(dir, fileExited), nil, 0o600)
		return err
	}
	var exitErr error
	done := make(chan struct{})
	go func() { exitErr = cmd.Wait(); close(done) }()
	if err := agentProxy(dir); err != nil {
		fmt.Fprintln(qlog, "veems: agent proxy:", err) // clients fall back to the agent socket
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	tick := time.NewTicker(5 * time.Second)

	stop := func() {
		killVMProcs(dir)
		cmd.Process.Kill()
		<-done
	}
	var why string
loop:
	for {
		select {
		case <-done:
			why = "the guest shut down"
			if exitErr != nil {
				why = fmt.Sprintf("QEMU exited (%v); its log was %s", exitErr, lastLine(filepath.Join(dir, "qemu.log")))
			}
			break loop
		case <-sig:
			why = "it was deleted (veems rm, or the end of veems run)"
			stop()
			break loop
		case <-tick.C:
			if cur, err := loadMeta(id); err == nil {
				m = cur
			}
			if t, w := m.stopsAt(); w != "" && time.Now().After(t) {
				why = w
				stop()
				break loop
			}
		}
	}
	killVMProcs(dir) // whatever is left of the sandbox
	if !exists(filepath.Join(dir, fileReady)) {
		// Died while starting: leave the log for `veems new` to report.
		os.WriteFile(filepath.Join(dir, fileExited), nil, 0o600)
		return nil
	}
	if m.Pool == "" {
		bury(m, why)
	}
	return os.RemoveAll(dir)
}

// stopsAt is when the supervisor stops m, at its deadline or its idle limit,
// whichever comes first, and why. why is "" if it has neither.
func (m *Meta) stopsAt() (t time.Time, why string) {
	if !m.Deadline.IsZero() {
		t, why = m.Deadline, "it reached its --ttl limit"
	}
	if m.IdleSecs > 0 {
		if st, err := os.Stat(filepath.Join(vmDir(m.ID), fileActivity)); err == nil {
			idle := st.ModTime().Add(time.Duration(m.IdleSecs) * time.Second)
			if why == "" || idle.Before(t) {
				t, why = idle, fmt.Sprintf("it had no veems commands for %s (its --idle limit)", time.Duration(m.IdleSecs)*time.Second)
			}
		}
	}
	return t, why
}

func goneDir() string { return filepath.Join(vmsDir(), ".gone") }

// bury notes why a VM stopped, so commands that still name it can say so
// instead of "no such vm". Notes older than an hour are cleared away.
func bury(m *Meta, why string) {
	os.MkdirAll(goneDir(), 0o700)
	ents, _ := os.ReadDir(goneDir())
	for _, e := range ents {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > time.Hour {
			os.Remove(filepath.Join(goneDir(), e.Name()))
		}
	}
	os.WriteFile(filepath.Join(goneDir(), m.ID), []byte(m.Name+"\n"+why), 0o600)
}

// buried says why the VM ref (an id, a prefix of one, or a name) stopped
// lately, or "".
func buried(ref string) string {
	if ref == "" {
		return ""
	}
	ents, _ := os.ReadDir(goneDir())
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(goneDir(), e.Name()))
		name, why, _ := strings.Cut(string(b), "\n")
		if err == nil && (strings.HasPrefix(e.Name(), ref) || name == ref) {
			return fmt.Sprintf("vm %s is gone: %s", e.Name(), why)
		}
	}
	return ""
}

// supervising reports whether m's supervisor is running. The PID alone is
// not enough: after a crash or a reboot it may belong to something else.
func supervising(m *Meta) bool {
	if m.PID <= 0 {
		return false
	}
	cl, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", m.PID))
	return err == nil && bytes.Contains(cl, []byte("\x00_supervise\x00"+m.ID+"\x00"))
}

func destroy(id string) error {
	m, err := loadMeta(id)
	if err != nil {
		if exists(vmDir(id)) {
			return os.RemoveAll(vmDir(id))
		}
		return err
	}
	if supervising(m) {
		syscall.Kill(m.PID, syscall.SIGTERM)
		for i := 0; i < 100 && supervising(m); i++ {
			time.Sleep(20 * time.Millisecond)
		}
		if supervising(m) {
			syscall.Kill(m.PID, syscall.SIGKILL)
		}
	}
	killVMProcs(vmDir(id))
	return os.RemoveAll(vmDir(id))
}

// listVMs returns live VMs and sweeps away directories whose supervisor is gone.
func listVMs() ([]*Meta, error) {
	ents, err := os.ReadDir(vmsDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	var out []*Meta
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		m, err := loadMeta(e.Name())
		info, _ := e.Info()
		starting := info != nil && time.Since(info.ModTime()) < time.Minute
		dead := err != nil || (m.PID != 0 && !supervising(m)) || (m.PID == 0 && !starting)
		if dead {
			// The supervisor died without cleaning up (it was SIGKILLed, say),
			// or a start was abandoned.
			if err == nil || !starting {
				killVMProcs(vmDir(e.Name()))
				os.RemoveAll(vmDir(e.Name()))
			}
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out, nil
}

// resolve accepts an id, a unique id prefix, or a name.
func resolve(ref string) (string, error) {
	if exists(metaPath(ref)) {
		return ref, nil
	}
	vms, _ := listVMs()
	var hits []string
	for _, m := range vms {
		if m.Pool == "" && (m.Name == ref || strings.HasPrefix(m.ID, ref)) {
			hits = append(hits, m.ID)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		if gone := buried(ref); gone != "" {
			return "", errors.New(gone)
		}
		return "", fmt.Errorf("no such vm: %s", ref)
	}
	return "", fmt.Errorf("%q matches several vms: %s", ref, strings.Join(hits, ", "))
}
