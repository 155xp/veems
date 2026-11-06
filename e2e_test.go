package main

// End-to-end tests: real `veems` commands, real KVM VMs. They need a built
// linux image (`veems build linux`) and run only with VEEMS_E2E=1:
//
//	VEEMS_E2E=1 go test -run E2E -v .
//
// They create a project and a pool for that project only, under a unique
// name, so they never hand out or remove anyone else's VMs.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	e2eBin     string
	e2eBinErr  error
	e2eBinOnce sync.Once
)

func veemsBin(t *testing.T) string {
	if os.Getenv("VEEMS_E2E") == "" {
		t.Skip("set VEEMS_E2E=1 to run against real VMs")
	}
	e2eBinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "veems-e2e-")
		if err != nil {
			e2eBinErr = err
			return
		}
		e2eBin = filepath.Join(dir, "veems")
		out, err := exec.Command("go", "build", "-o", e2eBin, ".").CombinedOutput()
		if err != nil {
			e2eBinErr = fmt.Errorf("go build: %v\n%s", err, out)
		}
	})
	if e2eBinErr != nil {
		t.Fatal(e2eBinErr)
	}
	return e2eBin
}

func veems(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command(veemsBin(t), args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("veems %s: %v\n%s%s", strings.Join(args, " "), err, out, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

type newReply struct {
	ID       string `json:"id"`
	FromPool bool   `json:"from_pool"`
	Project  string `json:"project"`
	ReadyMS  int    `json:"ready_ms"`
}

func newJSON(t *testing.T, args ...string) newReply {
	t.Helper()
	var r newReply
	if err := json.Unmarshal([]byte(veems(t, append([]string{"new", "--json"}, args...)...)), &r); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { exec.Command(veemsBin(t), "rm", r.ID).Run() })
	return r
}

func TestE2ENewExecWithoutPool(t *testing.T) {
	r := newJSON(t, "linux", "--no-pool")
	if r.FromPool {
		t.Fatal("--no-pool claimed a pooled vm")
	}
	if got := veems(t, "exec", r.ID, "echo hi"); !strings.HasPrefix(got, "hi") {
		t.Fatalf("exec: %q", got)
	}
	// Parallel commands on one VM share the agent through the supervisor.
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out, err := exec.Command(veemsBin(t), "exec", r.ID, fmt.Sprintf("sleep 0.3; echo %d", i)).Output()
			if err != nil || strings.TrimSpace(string(out)) != fmt.Sprint(i) {
				t.Errorf("parallel exec %d: %q %v", i, out, err)
			}
		}(i)
	}
	wg.Wait()
}

func TestE2EProjectAndPool(t *testing.T) {
	name := fmt.Sprintf("e2e-%d", time.Now().UnixNano()%1e9)
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "hello.txt"), []byte("from the host\n"), 0o644)
	veems(t, "project", "add", name, "linux", "--src", src, "--setup", "echo set-up > done.txt")
	t.Cleanup(func() {
		exec.Command(veemsBin(t), "pool", "--project", name, "0").Run()
		exec.Command(veemsBin(t), "project", "rm", name).Run()
	})
	dest := "/work/" + filepath.Base(src)

	// Without a pool: restored from the project snapshot, offline.
	r := newJSON(t, "--project", name)
	if r.FromPool || r.Project != name {
		t.Fatalf("new --project: %+v", r)
	}
	if got := veems(t, "exec", r.ID, "--cwd", dest, "cat hello.txt done.txt"); got != "from the host\nset-up" {
		t.Fatalf("project files: %q", got)
	}
	if out, err := exec.Command(veemsBin(t), "exec", r.ID, "--timeout", "10s",
		"curl -sS -m 3 https://example.com >/dev/null").CombinedOutput(); err == nil {
		t.Fatalf("project vm without --net reached the internet: %s", out)
	}

	// With a pool: the claim is instant, and the pool refills behind it.
	veems(t, "pool", "--project", name, "1")
	c := newJSON(t, "--project", name, "--name", name+"-vm")
	if !c.FromPool {
		t.Fatalf("expected a pooled vm: %+v", c)
	}
	if got := veems(t, "exec", name+"-vm", "cat "+dest+"/done.txt"); got != "set-up" {
		t.Fatalf("claimed vm: %q", got)
	}
	if strings.Contains(veems(t, "ls"), poolKey("linux", name, false)) {
		t.Fatal("ls shows waiting vms")
	}
	ready := func() []string {
		var rows []struct {
			Key   string
			Ready []string
		}
		json.Unmarshal([]byte(veems(t, "pool", "--json")), &rows)
		for _, row := range rows {
			if row.Key == poolKey("linux", name, false) {
				return row.Ready
			}
		}
		return nil
	}
	waitFor := func(what string, ok func([]string) bool) []string {
		for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
			if r := ready(); ok(r) {
				return r
			}
		}
		t.Fatalf("pool never %s", what)
		return nil
	}
	before := waitFor("refilled after the claim", func(r []string) bool { return len(r) == 1 && r[0] != c.ID })

	// Re-preparing the project makes the waiting VM stale; the pool replaces it.
	veems(t, "project", "refresh", name)
	waitFor("replaced the stale vm", func(r []string) bool { return len(r) == 1 && r[0] != before[0] })
	if exists(vmDir(before[0])) {
		t.Fatal("stale pooled vm still running")
	}
	veems(t, "pool", "--project", name, "0")
	if r := ready(); len(r) != 0 {
		t.Fatalf("pool not drained: %v", r)
	}
}

func TestE2EConcurrentClaims(t *testing.T) {
	name := fmt.Sprintf("e2e-c%d", time.Now().UnixNano()%1e9)
	veems(t, "project", "add", name, "linux", "--setup", "true")
	t.Cleanup(func() {
		exec.Command(veemsBin(t), "pool", "--project", name, "0").Run()
		exec.Command(veemsBin(t), "project", "rm", name).Run()
	})
	veems(t, "pool", "--project", name, "2")
	ids := make([]string, 4)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out, err := exec.Command(veemsBin(t), "new", "--project", name).Output()
			if err != nil {
				t.Errorf("new: %v", err)
				return
			}
			ids[i] = strings.TrimSpace(string(out))
		}(i)
	}
	wg.Wait()
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" {
			continue
		}
		t.Cleanup(func() { exec.Command(veemsBin(t), "rm", id).Run() })
		if seen[id] {
			t.Fatalf("vm %s handed out twice: %v", id, ids)
		}
		seen[id] = true
	}
	if len(seen) != len(ids) {
		t.Fatalf("got %v", ids)
	}
}
