package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func tempHome(t *testing.T) string {
	h := t.TempDir()
	t.Setenv("VEEMS_HOME", h)
	return h
}

// fakeAgent answers guest-agent requests for the VM in dir: syncs, pings,
// set-time. It counts connections so tests can see whether callers reconnected.
func fakeAgent(t *testing.T, dir string) *atomic.Int32 {
	os.MkdirAll(runDir(dir), 0o700)
	ln, err := net.Listen("unix", qgaSock(dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var conns atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns.Add(1)
			go func() {
				defer c.Close()
				br := bufio.NewReader(c)
				for {
					line, err := br.ReadBytes('\n')
					if err != nil {
						return
					}
					line = []byte(strings.TrimLeft(string(line), "\xff"))
					var req struct {
						Execute   string
						Arguments struct{ ID int64 }
					}
					json.Unmarshal(line, &req)
					switch req.Execute {
					case "guest-sync-delimited":
						fmt.Fprintf(c, "junk from an earlier client\n\xff{\"return\": %d}\n", req.Arguments.ID)
					case "guest-ping":
						fmt.Fprintf(c, "{\"return\": {}}\n")
					case "guest-set-time": // as on the Debian image: the clock is set, then hwclock is missing
						fmt.Fprintf(c, "{\"error\": {\"class\": \"GenericError\", \"desc\": \"failed to exec '/sbin/hwclock'\"}}\n")
					default:
						fmt.Fprintf(c, "{\"error\": {\"class\": \"CommandNotFound\", \"desc\": \"no %s\"}}\n", req.Execute)
					}
				}
			}()
		}
	}()
	return &conns
}

func TestAgentConnReusesOneConnection(t *testing.T) {
	dir := t.TempDir()
	conns := fakeAgent(t, dir)
	a := &agentConn{dir: dir}
	for i := 0; i < 5; i++ {
		if err := a.call("guest-ping", nil, nil, time.Second); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.call("guest-nope", nil, nil, time.Second); err == nil || !strings.Contains(err.Error(), "no guest-nope") {
		t.Fatalf("want the agent's error, got %v", err)
	}
	a.close()
	// The error closed the connection, so: one for the pings, none since.
	if n := conns.Load(); n != 1 {
		t.Fatalf("%d connections, want 1", n)
	}
}

func TestAgentProxyKeepsTheGuestConnection(t *testing.T) {
	dir := t.TempDir()
	conns := fakeAgent(t, dir)
	if err := agentProxy(dir); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				a := &agentConn{dir: dir}
				if err := a.call("guest-ping", nil, nil, 2*time.Second); err != nil {
					t.Error(err)
				}
				a.close()
			}
		}()
	}
	wg.Wait()
	if n := conns.Load(); n != 1 {
		t.Fatalf("guest agent saw %d connections, want 1", n)
	}
}

func TestTakeIsExclusive(t *testing.T) {
	tempHome(t)
	m := &Meta{ID: "lnx-take01"}
	os.MkdirAll(vmDir(m.ID), 0o700)
	os.WriteFile(filepath.Join(vmDir(m.ID), "pooled"), nil, 0o600)
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if take(m) {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d claims succeeded, want exactly 1", wins.Load())
	}
}

// fakePooled makes a waiting VM: a live stand-in supervisor, a meta file, the
// pooled marker, and an agent that answers.
func fakePooled(t *testing.T, id, pool, source string) {
	sup := exec.Command("sh", "-c", "sleep 60; true", "_supervise", id)
	if err := sup.Start(); err != nil {
		t.Fatal(err)
	}
	go sup.Wait()
	t.Cleanup(func() { sup.Process.Kill() })
	dir := vmDir(id)
	os.MkdirAll(dir, 0o700)
	fakeAgent(t, dir)
	os.WriteFile(filepath.Join(dir, "activity"), nil, 0o600)
	os.WriteFile(filepath.Join(dir, "pooled"), nil, 0o600)
	if err := saveMeta(&Meta{ID: id, OS: "linux", CPUs: 2, MemMB: 1024, Pool: pool, Source: source,
		PID: sup.Process.Pid, Created: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
}

func TestClaimSkipsStaleAndSetsLimits(t *testing.T) {
	tempHome(t)
	os.MkdirAll(imageDir("linux"), 0o755)
	state := tmplState("linux")
	os.WriteFile(state, []byte("state"), 0o644)
	os.WriteFile(tmplDisk("linux"), nil, 0o644)
	os.WriteFile(tmplMeta("linux"), []byte(`{"cpus":2,"mem_mb":1024}`), 0o644)
	src := imageSource(profiles["linux"])
	fakePooled(t, "lnx-stale1", "linux", "an older snapshot")
	fakePooled(t, "lnx-net001", "linux+net", src.stamp())
	fakePooled(t, "lnx-fresh1", "linux", src.stamp())

	m := claim(NewOpts{OS: "linux", Name: "mine", TTL: time.Hour, Idle: 5 * time.Minute}, src)
	if m == nil || m.ID != "lnx-fresh1" {
		t.Fatalf("claimed %+v, want lnx-fresh1", m)
	}
	if exists(vmDir("lnx-stale1")) {
		t.Error("stale pooled vm was not removed")
	}
	if exists(filepath.Join(vmDir("lnx-fresh1"), "pooled")) {
		t.Error("claimed vm still marked pooled")
	}
	if !exists(filepath.Join(vmDir("lnx-net001"), "pooled")) {
		t.Error("a no-network request took a networked vm")
	}
	saved, err := loadMeta("lnx-fresh1")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Pool != "" || saved.Name != "mine" || saved.IdleSecs != 300 || time.Until(saved.Deadline) < 59*time.Minute {
		t.Errorf("claimed meta not updated: %+v", saved)
	}
	if id, err := resolve("mine"); err != nil || id != "lnx-fresh1" {
		t.Errorf("resolve(mine) = %q, %v", id, err)
	}
	if _, err := resolve("lnx-net"); err == nil {
		t.Error("a waiting vm should not resolve by prefix")
	}
	if claim(NewOpts{OS: "linux"}, src) != nil {
		t.Error("claimed from an empty pool")
	}
}

func TestPoolsFile(t *testing.T) {
	tempHome(t)
	offline := PoolTarget{Image: "linux", Project: "app", Size: 1}
	online := PoolTarget{Image: "linux", Project: "app", Net: true, Size: 3}
	if err := savePools([]PoolTarget{{Image: "linux", Size: 2}, online}); err != nil {
		t.Fatal(err)
	}
	if tg, ok := poolTarget(PoolTarget{Image: "linux"}.key()); !ok || tg.Size != 2 {
		t.Errorf("linux pool: %+v %v", tg, ok)
	}
	if tg, ok := poolTarget(online.key()); !ok || tg.Size != 3 {
		t.Errorf("networked project pool: %+v %v", tg, ok)
	}
	if _, ok := poolTarget(offline.key()); ok {
		t.Error("networked and offline pools must be separate")
	}
}

func TestStampChangesOnRewrite(t *testing.T) {
	f := filepath.Join(t.TempDir(), "snap.state")
	os.WriteFile(f, []byte("one"), 0o644)
	a := stamp(f)
	time.Sleep(10 * time.Millisecond)
	os.WriteFile(f+".new", []byte("two"), 0o644)
	os.Rename(f+".new", f)
	if b := stamp(f); a == b || a == "" {
		t.Fatalf("stamp did not change: %q -> %q", a, b)
	}
}

func TestProjectNames(t *testing.T) {
	for _, ok := range []string{"app", "my-app.v2", "a_1"} {
		if checkProjectName(ok) != nil {
			t.Errorf("%q rejected", ok)
		}
	}
	for _, bad := range []string{"", "../x", "a/b", "App", ".hidden", "a..b", strings.Repeat("a", 41)} {
		if checkProjectName(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestDefaultDest(t *testing.T) {
	lin, win := profiles["linux"], profiles["windows"]
	for _, c := range []struct {
		p    *Profile
		o    PrepOpts
		want string
	}{
		{lin, PrepOpts{Name: "x", Git: "https://example.com/org/repo.git"}, "/work/repo"},
		{lin, PrepOpts{Name: "x", Git: "https://example.com/org/repo/"}, "/work/repo"},
		{lin, PrepOpts{Name: "x", Src: "/home/someone/code/site"}, "/work/site"},
		{lin, PrepOpts{Name: "tools", Setup: "apt-get install -y foo"}, "/work/tools"},
		{win, PrepOpts{Name: "x", Git: "https://example.com/org/app.git"}, `C:\work\app`},
	} {
		if got := defaultDest(c.p, c.o); got != c.want {
			t.Errorf("defaultDest(%+v) = %q, want %q", c.o, got, c.want)
		}
	}
}

func TestNetworkIsDetachable(t *testing.T) {
	a := strings.Join(qemuArgs(Spec{OS: "linux", Dir: "/d", Disk: "/d/disk", CPUs: 2, MemMB: 1024, Net: true}), " ")
	// The NIC must sit on the same hub port as a VM with no network, so the
	// saved device state restores into either kind of VM.
	off := strings.Join(qemuArgs(Spec{OS: "linux", Dir: "/d", Disk: "/d/disk", CPUs: 2, MemMB: 1024}), " ")
	if !strings.Contains(a, "hubport,id=net0,hubid=0") || !strings.Contains(off, "hubport,id=net0,hubid=0") {
		t.Fatalf("NIC peer differs:\n%s\n%s", a, off)
	}
	if !strings.Contains(a, "user,id=uplink,") || !strings.Contains(a, "hubport,id=uplinkport,hubid=0,netdev=uplink") {
		t.Fatalf("no detachable uplink: %s", a)
	}
	if strings.Contains(off, "user,") {
		t.Fatalf("offline VM has a user-mode network: %s", off)
	}
}
