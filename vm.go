package main

import (
	"bufio"
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
	Source   string    `json:"source,omitempty"`  // stamp of the snapshot or disk it started from
	Pool     string    `json:"pool,omitempty"`    // waiting in this pool, not yet claimed
	Pooled   bool      `json:"-"`                 // this process claimed it from a pool
	NetHub   bool      `json:"net_hub,omitempty"` // network on a detachable hub (project prep)
}

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
	tmp := metaPath(m.ID) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, metaPath(m.ID))
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
	Quiet   bool
	Project string // start from this project's snapshot
	NoPool  bool   // never claim a pooled VM
	Pool    string // start a VM for this pool
	NetHub  bool   // with Net: network that can be unplugged before a snapshot
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

func createVM(o NewOpts) (*Meta, error) {
	src, err := sourceFor(o)
	if err != nil {
		return nil, err
	}
	p := src.prof
	if o.Pool == "" {
		key := poolKey(p.Key, src.project, o.Net)
		defer refill(key)
		if !o.Cold && !o.NoPool {
			if m := claim(o, src); m != nil {
				return m, nil
			}
		}
	}
	if o.CPUs == 0 {
		o.CPUs = p.CPUs
	}
	if o.MemMB == 0 {
		o.MemMB = p.MemMB
	}
	listVMs() // sweeps up after crashed supervisors before counting memory
	if err := checkHostMemory(o.MemMB); err != nil {
		if o.Pool == "" && len(pooled("")) > 0 {
			err = fmt.Errorf("%w; VMs waiting in a pool hold memory too (`veems pool` shows them)", err)
		}
		return nil, err
	}
	snap := src.snap
	useSnap := !o.Cold && snap != nil && snap.CPUs == o.CPUs && snap.MemMB == o.MemMB && snap.QEMU == qemuVersion()
	if !o.Cold && snap != nil && !useSnap && !o.Quiet {
		fix := "`veems build " + p.Key + " --snapshot` refreshes it"
		if src.project != "" {
			fix = "the project was saved with " + fmt.Sprintf("%d cpus and %d MB", snap.CPUs, snap.MemMB)
		}
		fmt.Fprintf(os.Stderr, "veems: snapshot does not match (cpus/mem/qemu version), cold booting; %s\n", fix)
	}

	m := &Meta{ID: newID(p.Prefix), Name: o.Name, OS: p.OS, Desktop: p.Desktop, CPUs: o.CPUs, MemMB: o.MemMB, Net: o.Net,
		Snapshot: useSnap, Created: time.Now().UTC(), IdleSecs: int(o.Idle.Seconds()), Project: src.project, Pool: o.Pool, NetHub: o.Net && o.NetHub}
	if o.Pool != "" {
		o.TTL, m.IdleSecs = 0, 0 // a waiting VM gets its limits when it is claimed
	}
	if o.TTL > 0 {
		m.Deadline = m.Created.Add(o.TTL)
	}
	dir := vmDir(m.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	fail := func(err error) (*Meta, error) { os.RemoveAll(dir); return nil, err }
	if !useSnap {
		src.snap = nil
	}
	if src.project == "" && !useSnap {
		src.disk = basePath(p.Key) // the image snapshot's disk only goes with its RAM state
	}
	m.Source = src.stamp()
	if err := qemuImgCreate(filepath.Join(dir, "disk.qcow2"), src.disk, 0); err != nil {
		return fail(err)
	}
	os.WriteFile(filepath.Join(dir, "activity"), nil, 0o600)
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

	wait := time.Duration(p.Boot) * time.Second
	if useSnap {
		wait = 60 * time.Second
	}
	deadline := time.Now().Add(wait)
	resumed := false
	var pausedAt time.Time
	for {
		if !exists(dir) || exists(filepath.Join(dir, "exited")) {
			log, _ := os.ReadFile(filepath.Join(dir, "qemu.log"))
			os.RemoveAll(dir)
			return nil, fmt.Errorf("vm failed to start:\n%s", strings.TrimSpace(string(log)))
		}
		if useSnap && !resumed {
			// The snapshot was saved paused, and QEMU restores that run state.
			if raw, err := qmp(dir, "query-status", nil, 2*time.Second); err == nil {
				var st struct{ Status string }
				json.Unmarshal(raw, &st)
				if st.Status == "paused" && pausedAt.IsZero() {
					pausedAt = time.Now()
				}
				// Resume once the agent proxy is connected (see agentProxy).
				if st.Status == "paused" && (exists(filepath.Join(dir, "agent.sock")) || time.Since(pausedAt) > 2*time.Second) {
					if _, err := qmp(dir, "cont", nil, 5*time.Second); err == nil {
						resumed = true
					}
				} else if st.Status == "running" {
					resumed = true
				}
			}
		} else if a := guestSession(m.ID); a.call("guest-ping", nil, nil, 2*time.Second) == nil {
			// A restored snapshot wakes up with the clock it was saved with.
			// Same connection: a new one would wait out the agent's reconnect pause.
			a.call("guest-set-time", map[string]any{"time": time.Now().UnixNano()}, nil, 10*time.Second)
			a.close()
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
	os.WriteFile(filepath.Join(dir, "ready"), nil, 0o600)
	return m, nil
}

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
				return fmt.Errorf("not enough free memory: %d MB available, VM needs %d MB plus %d MB kept for the host (VEEMS_RESERVE_MB); remove idle VMs with `veems rm --all`", avail, memMB, reserve)
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
		Net: m.Net, NetHub: m.NetHub, ROBinds: src.binds}
	if m.Snapshot {
		s.Incoming = src.state
	}
	cmd, err := launchCmd(s)
	if err != nil {
		os.WriteFile(filepath.Join(dir, "qemu.log"), []byte(err.Error()), 0o600)
		os.WriteFile(filepath.Join(dir, "exited"), nil, 0o600)
		return err
	}
	logf, _ := os.Create(filepath.Join(dir, "qemu.log"))
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		os.WriteFile(filepath.Join(dir, "exited"), nil, 0o600)
		return err
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	if err := agentProxy(dir); err != nil {
		fmt.Fprintln(logf, "veems: agent proxy:", err) // clients fall back to the agent socket
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	tick := time.NewTicker(5 * time.Second)

	stop := func() {
		killVMProcs(dir)
		cmd.Process.Kill()
		<-done
	}
loop:
	for {
		select {
		case <-done:
			break loop
		case <-sig:
			stop()
			break loop
		case <-tick.C:
			if cur, err := loadMeta(id); err == nil {
				m = cur
			}
			if !m.Deadline.IsZero() && time.Now().After(m.Deadline) {
				stop()
				break loop
			}
			if m.IdleSecs > 0 && exists(filepath.Join(dir, "ready")) {
				if st, err := os.Stat(filepath.Join(dir, "activity")); err == nil &&
					time.Since(st.ModTime()) > time.Duration(m.IdleSecs)*time.Second {
					stop()
					break loop
				}
			}
		}
	}
	killVMProcs(dir) // whatever is left of the sandbox
	if !exists(filepath.Join(dir, "ready")) {
		// Died while starting: leave the log for `veems new` to report.
		os.WriteFile(filepath.Join(dir, "exited"), nil, 0o600)
		return nil
	}
	return os.RemoveAll(dir)
}

func alive(pid int) bool {
	return pid > 0 && syscall.Kill(pid, 0) == nil
}

// destroy stops a VM and removes everything it wrote.
func destroy(id string) error {
	m, err := loadMeta(id)
	if err != nil {
		if exists(vmDir(id)) {
			return os.RemoveAll(vmDir(id))
		}
		return err
	}
	if alive(m.PID) {
		syscall.Kill(m.PID, syscall.SIGTERM)
		for i := 0; i < 100 && alive(m.PID); i++ {
			time.Sleep(20 * time.Millisecond)
		}
		if alive(m.PID) {
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
		m, err := loadMeta(e.Name())
		info, _ := e.Info()
		starting := info != nil && time.Since(info.ModTime()) < time.Minute
		dead := err != nil || (m.PID != 0 && !alive(m.PID)) || (m.PID == 0 && !starting)
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
		return "", fmt.Errorf("no such vm: %s", ref)
	}
	return "", fmt.Errorf("%q matches several vms: %s", ref, strings.Join(hits, ", "))
}
