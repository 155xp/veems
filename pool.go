package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// The warm pool keeps VMs restored and waiting so `veems new` can hand one
// out at once. It is optional and has no daemon: `veems pool` sets a size and
// fills it, and every claim starts a short-lived background `veems` that tops
// the pool back up and exits. With no pool, or an empty one, `veems new`
// restores a VM as usual.
//
// A pooled VM is an ordinary VM with Meta.Pool set and a filePooled marker in
// its directory. Removing the marker is the claim: unlink succeeds for exactly
// one process.

// PoolTarget is one pool: how many VMs of one kind to keep ready.
type PoolTarget struct {
	Image   string `json:"image"` // profile key, e.g. linux
	Project string `json:"project,omitempty"`
	Net     bool   `json:"net,omitempty"`
	Size    int    `json:"size"`
}

func (t PoolTarget) key() string { return poolKey(t.Image, t.Project, t.Net) }

func poolKey(image, project string, net bool) string {
	k := image
	if project != "" {
		k = "project:" + project
	}
	if net {
		k += "+net"
	}
	return k
}

func poolsFile() string { return filepath.Join(veemsHome(), "pools.json") }

func loadPools() []PoolTarget {
	b, err := os.ReadFile(poolsFile())
	if err != nil {
		return nil
	}
	var ts []PoolTarget
	json.Unmarshal(b, &ts)
	return ts
}

func savePools(ts []PoolTarget) error {
	sort.Slice(ts, func(i, j int) bool { return ts[i].key() < ts[j].key() })
	b, _ := json.MarshalIndent(ts, "", "  ")
	os.MkdirAll(veemsHome(), 0o755)
	return writeAtomic(poolsFile(), b, 0o644)
}

func poolTarget(key string) (PoolTarget, bool) {
	for _, t := range loadPools() {
		if t.key() == key {
			return t, true
		}
	}
	return PoolTarget{}, false
}

// pooled lists the waiting VMs of one pool (all pools if key is ""), oldest first.
func pooled(key string) []*Meta {
	vms, _ := listVMs()
	var out []*Meta
	for _, m := range vms {
		if m.Pool != "" && (key == "" || m.Pool == key) && exists(filepath.Join(vmDir(m.ID), filePooled)) {
			out = append(out, m)
		}
	}
	return out
}

// take claims a waiting VM so no one else can. False if someone was faster.
func take(m *Meta) bool { return os.Remove(filepath.Join(vmDir(m.ID), filePooled)) == nil }

// claim hands out a waiting VM that matches o and src, or nil.
func claim(o NewOpts, src *source) *Meta {
	key := poolKey(src.prof.Key, src.project, o.Net)
	want := src.stamp()
	for _, m := range pooled(key) {
		if (o.CPUs != 0 && o.CPUs != m.CPUs) || (o.MemMB != 0 && o.MemMB != m.MemMB) || !take(m) {
			continue
		}
		if m.Source != want {
			destroy(m.ID) // made from an older image or project
			continue
		}
		m.Pool = ""
		m.limit(o)
		touch(m.ID)
		// It has waited a while: check that it still answers.
		err := wake(m.ID, 5*time.Second)
		if err == nil {
			err = saveMeta(m)
		}
		if err != nil {
			destroy(m.ID)
			continue
		}
		m.Pooled = true
		return m
	}
	return nil
}

// refill starts a background `veems _pool-fill` if key has a pool.
func refill(key string) {
	if _, ok := poolTarget(key); !ok {
		return
	}
	self, err := os.Executable()
	if err != nil {
		return
	}
	cmd := exec.Command(self, "_pool-fill")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if log, err := os.OpenFile(filepath.Join(veemsHome(), "pool.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		cmd.Stdout, cmd.Stderr = log, log
		defer log.Close()
	}
	if cmd.Start() == nil {
		cmd.Process.Release()
	}
}

// fillPools brings every pool to its size: it removes waiting VMs that are
// stale or extra, re-prepares projects whose image was rebuilt, and restores
// new VMs until each pool is full or the host is short on memory. Fillers
// queue on a lock, so the last one sees the latest state.
func fillPools(verbose bool) error {
	unlock, err := lockPools()
	if err != nil {
		return err
	}
	defer unlock()
	say := func(format string, a ...any) {
		if verbose {
			logf(format, a...)
		} else {
			fmt.Fprintf(os.Stderr, "%s veems: "+format+"\n", append([]any{time.Now().Format(time.RFC3339)}, a...)...)
		}
	}
	targets := map[string]PoolTarget{}
	for _, t := range loadPools() {
		targets[t.key()] = t
	}
	// Remove what no pool wants any more.
	have := map[string][]*Meta{}
	for _, m := range pooled("") {
		t, ok := targets[m.Pool]
		if !ok {
			if take(m) {
				destroy(m.ID)
			}
			continue
		}
		src, err := poolSource(t)
		if err != nil || m.Source != src.stamp() {
			if take(m) {
				say("removing stale pooled vm %s", m.ID)
				destroy(m.ID)
			}
			continue
		}
		have[m.Pool] = append(have[m.Pool], m)
	}
	var firstErr error
	for _, key := range sortedKeys(targets) {
		t := targets[key]
		for len(have[key]) > t.Size {
			m := have[key][len(have[key])-1]
			have[key] = have[key][:len(have[key])-1]
			if take(m) {
				destroy(m.ID)
			}
		}
		if t.Project != "" && len(have[key]) < t.Size {
			if err := freshenProject(t.Project, say); err != nil {
				say("pool %s: %v", key, err)
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
		}
		for n := len(have[key]); n < t.Size; n++ {
			start := time.Now()
			var m *Meta
			src, err := poolSource(t)
			if err == nil {
				// A waiting VM gets its limits when it is claimed.
				m, err = startVM(src, NewOpts{Net: t.Net, Pool: key})
			}
			if err != nil {
				say("pool %s: %v", key, err)
				if firstErr == nil {
					firstErr = err
				}
				break
			}
			if err := os.WriteFile(filepath.Join(vmDir(m.ID), filePooled), nil, 0o600); err != nil {
				destroy(m.ID)
				break
			}
			say("pool %s: %s ready in %s", key, m.ID, time.Since(start).Round(time.Millisecond))
		}
	}
	return firstErr
}

// freshenProject re-prepares a project whose image snapshot changed, from its
// recorded recipe.
func freshenProject(name string, say func(string, ...any)) error {
	pj, err := loadProject(name)
	if err != nil {
		return err
	}
	why := pj.stale()
	if why == "" {
		return nil
	}
	say("project %s is out of date (%s), preparing it again", name, why)
	po, err := pj.prepOpts()
	if err != nil {
		return err
	}
	_, err = prepare(po)
	return err
}

func poolSource(t PoolTarget) (*source, error) {
	p := profiles[t.Image]
	if p == nil {
		return nil, fmt.Errorf("unknown image %q", t.Image)
	}
	return sourceFor(NewOpts{OS: p.OS, Desktop: p.Desktop, Project: t.Project})
}

func sortedKeys(m map[string]PoolTarget) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// lockPools serializes pool changes and fills across processes.
func lockPools() (unlock func(), err error) {
	os.MkdirAll(veemsHome(), 0o755)
	lock, err := lockFile(filepath.Join(veemsHome(), "pool.lock"), true)
	if err != nil {
		return nil, err
	}
	return func() { lock.Close() }, nil
}

// setPool changes one pool's size (0 removes it) and fills or drains it now.
func setPool(t PoolTarget) error {
	unlock, err := lockPools()
	if err != nil {
		return err
	}
	var ts []PoolTarget
	for _, cur := range loadPools() {
		if cur.key() != t.key() {
			ts = append(ts, cur)
		}
	}
	if t.Size > 0 {
		ts = append(ts, t)
	}
	err = savePools(ts)
	unlock()
	if err != nil {
		return err
	}
	return fillPools(true)
}

func showPools(asJSON bool) error {
	ts := loadPools()
	type row struct {
		PoolTarget
		Key   string   `json:"key"`
		Ready []string `json:"ready"`
	}
	var rows []row
	for _, t := range ts {
		r := row{PoolTarget: t, Key: t.key(), Ready: []string{}}
		for _, m := range pooled(t.key()) {
			r.Ready = append(r.Ready, m.ID)
		}
		rows = append(rows, r)
	}
	if asJSON {
		if rows == nil {
			rows = []row{}
		}
		printJSON(rows)
		return nil
	}
	if len(rows) == 0 {
		fmt.Fprintln(os.Stderr, "no pools (veems pool linux 2 keeps two linux VMs ready)")
		return nil
	}
	for _, r := range rows {
		fmt.Printf("%-24s %d/%d ready  %s\n", r.Key, len(r.Ready), r.Size, strings.Join(r.Ready, " "))
	}
	return nil
}
