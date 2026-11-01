// veems: throwaway Linux and Windows VMs for agents, one command each.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

const version = "0.1.0"

const usage = `veems: fresh, sandboxed Linux/Windows VMs for agents.

Use linux unless the task needs Windows. A Linux VM uses about a third of
the memory and starts faster.

  veems new linux                 start a fresh VM, print its id (about 1 s)
  veems exec <vm> 'uname -a'      run a shell line (bash; PowerShell on Windows)
  veems exec <vm> -- python3 -V   run a program with arguments, no shell
  veems cp ./src <vm>:dest        copy a file or directory in
  veems cp <vm>:path ./out        copy a file or directory out
  veems rm <vm>                   stop it and delete everything
  veems run linux -- make test    new + exec + rm in one step

Every VM starts from the same clean snapshot, and nothing it does can reach
your machine: no shared folders, no network unless you pass --net, and even
then no access to this host or the LAN, only the internet.

VMs remove themselves after 20 min with no veems commands (--idle) or 2 h in
total (--ttl), whichever comes first. Still run 'veems rm' when you are done.

Commands
  new <linux|windows> [--net] [--name N] [--ttl 2h] [--idle 20m] [--cpus N] [--mem MB] [--cold] [--json]
  exec <vm> [--cwd DIR] [--env K=V]... [--timeout 10m] [--stdin] [--json] ('line' | -- prog args...)
  cp <src> <dst>         one side is <vm>:path; a relative or empty path is
                         under the work dir (/work, C:\work)
  run <linux|windows> [new flags] [--push LOCAL:REMOTE]... [--pull REMOTE:LOCAL]... ('line' | -- prog args...)
  ls [--json]            list running VMs
  rm <vm>... | --all     destroy VMs
  ttl <vm> [--ttl D] [--idle D]   change a running VM's limits (0 = no limit)
  screenshot <vm> out.png         capture the screen (PNG)
  build <linux|windows> [--snapshot] [--iso PATH] [--desktop]   one-time image build
  doctor                 check this host
  skill [show|install]   print the agent skill, or install it for Claude
                         Code, Codex, OpenCode, oh-my-pi, and pi

<vm> is an id like lnx-k3m9qa, a unique prefix of one, or a --name.
'exec' exits with the command's exit code, or 124 on timeout.
Network: with --net the VM can reach the internet only. Without it the VM
has no network at all.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	code := 0
	switch cmd {
	case "new", "create", "up":
		err = cmdNew(args)
	case "exec", "x":
		code, err = cmdExec(args)
	case "cp", "copy":
		err = cmdCp(args)
	case "run":
		code, err = cmdRun(args)
	case "ls", "list", "ps":
		err = cmdLs(args)
	case "rm", "kill", "delete", "down":
		err = cmdRm(args)
	case "ttl", "extend":
		err = cmdTTL(args)
	case "screenshot":
		err = cmdScreenshot(args)
	case "build":
		err = cmdBuild(args)
	case "doctor":
		err = cmdDoctor()
	case "skill":
		err = cmdSkill(args)
	case "version", "--version":
		fmt.Println("veems", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	case "_supervise":
		err = supervise(args[0])
	default:
		err = fmt.Errorf("unknown command %q (see `veems help`)", cmd)
		code = 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "veems:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// flags is a tiny parser that allows flags anywhere before "--".
type flags struct {
	vals  map[string][]string
	bools map[string]bool
	pos   []string
	rest  []string // after "--"
	dash  bool
}

func parse(args []string, valued, boolean string) (*flags, error) {
	f := &flags{vals: map[string][]string{}, bools: map[string]bool{}}
	isV := map[string]bool{}
	isB := map[string]bool{}
	for _, n := range strings.Fields(valued) {
		isV[n] = true
	}
	for _, n := range strings.Fields(boolean) {
		isB[n] = true
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			f.rest, f.dash = args[i+1:], true
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			f.pos = append(f.pos, a)
			continue
		}
		name, val, hasVal := strings.Cut(strings.TrimLeft(a, "-"), "=")
		switch {
		case isB[name]:
			f.bools[name] = true
		case isV[name]:
			if !hasVal {
				if i+1 >= len(args) {
					return nil, fmt.Errorf("--%s needs a value", name)
				}
				i++
				val = args[i]
			}
			f.vals[name] = append(f.vals[name], val)
		default:
			return nil, fmt.Errorf("unknown flag %s", a)
		}
	}
	return f, nil
}

func (f *flags) str(name, def string) string {
	if v := f.vals[name]; len(v) > 0 {
		return v[len(v)-1]
	}
	return def
}

func (f *flags) dur(name string, def time.Duration) (time.Duration, error) {
	s := f.str(name, "")
	if s == "" {
		return def, nil
	}
	if s == "0" || s == "none" || s == "off" {
		return 0, nil
	}
	if n, err := strconv.Atoi(s); err == nil {
		return time.Duration(n) * time.Second, nil
	}
	return time.ParseDuration(s)
}

func (f *flags) num(name string) (int, error) {
	s := f.str(name, "")
	if s == "" {
		return 0, nil
	}
	return strconv.Atoi(s)
}

func envDur(name string, def time.Duration) time.Duration {
	if s := os.Getenv(name); s != "" {
		if d, err := time.ParseDuration(s); err == nil {
			return d
		}
	}
	return def
}

const newValued = "name ttl idle cpus mem"
const newBools = "net cold json"

func newOpts(f *flags, osName string) (NewOpts, error) {
	o := NewOpts{OS: osName, Name: f.str("name", ""), Net: f.bools["net"], Cold: f.bools["cold"]}
	var err error
	if o.TTL, err = f.dur("ttl", envDur("VEEMS_TTL", 2*time.Hour)); err != nil {
		return o, err
	}
	if o.Idle, err = f.dur("idle", envDur("VEEMS_IDLE", 20*time.Minute)); err != nil {
		return o, err
	}
	if o.CPUs, err = f.num("cpus"); err != nil {
		return o, err
	}
	if o.MemMB, err = f.num("mem"); err != nil {
		return o, err
	}
	return o, nil
}

func printJSON(v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
}

func cmdNew(args []string) error {
	f, err := parse(args, newValued, newBools)
	if err != nil {
		return err
	}
	if len(f.pos) != 1 {
		return fmt.Errorf("usage: veems new <linux|windows> [flags]")
	}
	o, err := newOpts(f, f.pos[0])
	if err != nil {
		return err
	}
	start := time.Now()
	m, err := createVM(o)
	if err != nil {
		return err
	}
	if f.bools["json"] {
		printJSON(map[string]any{"id": m.ID, "os": m.OS, "net": m.Net, "from_snapshot": m.Snapshot,
			"ready_ms": time.Since(start).Milliseconds(), "deadline": m.Deadline, "idle_secs": m.IdleSecs})
		return nil
	}
	fmt.Println(m.ID)
	return nil
}

// execTarget turns positionals and the part after "--" into ExecOpts.
func execTarget(f *flags, pos []string) (ExecOpts, error) {
	var o ExecOpts
	switch {
	case f.dash && len(f.rest) > 0:
		o.Argv = f.rest
	case len(pos) == 1:
		o.Line = pos[0]
	case len(pos) > 1:
		o.Argv = pos
	default:
		return o, fmt.Errorf("nothing to run: give 'a shell line' or -- program args")
	}
	o.Cwd = f.str("cwd", "")
	o.Env = f.vals["env"]
	var err error
	if o.Timeout, err = f.dur("timeout", 10*time.Minute); err != nil {
		return o, err
	}
	if f.bools["stdin"] {
		if o.Stdin, err = io.ReadAll(os.Stdin); err != nil {
			return o, err
		}
	}
	return o, nil
}

func emit(r *ExecResult, asJSON bool) int {
	if asJSON {
		printJSON(map[string]any{"exit_code": r.ExitCode, "stdout": string(r.Stdout), "stderr": string(r.Stderr),
			"timed_out": r.TimedOut, "truncated": r.Truncated})
		return r.ExitCode
	}
	os.Stdout.Write(r.Stdout)
	os.Stderr.Write(r.Stderr)
	if r.TimedOut {
		fmt.Fprintln(os.Stderr, "veems: command timed out and was killed (raise --timeout, or --timeout 0 for none)")
	}
	if r.Truncated {
		fmt.Fprintln(os.Stderr, "veems: output was over 16 MB and got cut; write it to a file and `veems cp` it out")
	}
	return r.ExitCode
}

const execValued = "cwd env timeout"
const execBools = "stdin json"

func cmdExec(args []string) (int, error) {
	f, err := parse(args, execValued, execBools)
	if err != nil {
		return 2, err
	}
	if len(f.pos) < 1 {
		return 2, fmt.Errorf("usage: veems exec <vm> ('line' | -- prog args...)")
	}
	id, err := resolve(f.pos[0])
	if err != nil {
		return 1, err
	}
	m, err := loadMeta(id)
	if err != nil {
		return 1, err
	}
	o, err := execTarget(f, f.pos[1:])
	if err != nil {
		return 2, err
	}
	r, err := guestExec(m, o)
	if err != nil {
		return 1, err
	}
	return emit(r, f.bools["json"]), nil
}

// splitRemote parses "<vm>:path"; ok is false for a local path.
func splitRemote(s string) (id, p string, ok bool) {
	ref, p, found := strings.Cut(s, ":")
	if !found || ref == "" || strings.ContainsAny(ref, `/\`) {
		return "", "", false
	}
	id, err := resolve(ref)
	if err != nil {
		return "", "", false
	}
	return id, p, true
}

func cmdCp(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: veems cp <src> <dst>  (one side is <vm>:path)")
	}
	sid, sp, sRemote := splitRemote(args[0])
	did, dp, dRemote := splitRemote(args[1])
	switch {
	case sRemote && dRemote:
		return fmt.Errorf("copy between two VMs: copy out to the host first")
	case dRemote:
		m, err := loadMeta(did)
		if err != nil {
			return err
		}
		return push(m, args[0], dp)
	case sRemote:
		m, err := loadMeta(sid)
		if err != nil {
			return err
		}
		return pull(m, sp, args[1])
	}
	return fmt.Errorf("neither %q nor %q names a running vm (write <vm>:path)", args[0], args[1])
}

func cmdRun(args []string) (int, error) {
	f, err := parse(args, newValued+" "+execValued+" push pull", newBools+" "+execBools+" keep")
	if err != nil {
		return 2, err
	}
	if len(f.pos) < 1 {
		return 2, fmt.Errorf("usage: veems run <linux|windows> [flags] ('line' | -- prog args...)")
	}
	no, err := newOpts(f, f.pos[0])
	if err != nil {
		return 2, err
	}
	eo, err := execTarget(f, f.pos[1:])
	if err != nil {
		return 2, err
	}
	no.Quiet = true
	m, err := createVM(no)
	if err != nil {
		return 1, err
	}
	if f.bools["keep"] {
		fmt.Fprintln(os.Stderr, "veems: vm", m.ID, "kept running")
	} else {
		defer destroy(m.ID)
	}
	for _, spec := range f.vals["push"] {
		local, remote, _ := strings.Cut(spec, ":")
		if err := push(m, local, remote); err != nil {
			return 1, fmt.Errorf("push %s: %w", spec, err)
		}
	}
	r, err := guestExec(m, eo)
	if err != nil {
		return 1, err
	}
	for _, spec := range f.vals["pull"] {
		remote, local, ok := cutLast(spec)
		if !ok {
			return 1, fmt.Errorf("--pull wants REMOTE:LOCAL, got %q", spec)
		}
		if err := pull(m, remote, local); err != nil {
			fmt.Fprintf(os.Stderr, "veems: pull %s: %v\n", spec, err)
		}
	}
	return emit(r, f.bools["json"]), nil
}

// cutLast splits REMOTE:LOCAL at the last colon so Windows paths (C:\x) survive.
func cutLast(s string) (string, string, bool) {
	i := strings.LastIndex(s, ":")
	if i <= 0 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}

func cmdLs(args []string) error {
	f, err := parse(args, "", "json")
	if err != nil {
		return err
	}
	vms, err := listVMs()
	if err != nil {
		return err
	}
	if f.bools["json"] {
		if vms == nil {
			vms = []*Meta{}
		}
		printJSON(vms)
		return nil
	}
	if len(vms) == 0 {
		fmt.Fprintln(os.Stderr, "no vms running")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tOS\tCPUS\tMEM\tNET\tAGE\tSTOPS IN")
	for _, m := range vms {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%dM\t%v\t%s\t%s\n", m.ID, m.Name, m.OS, m.CPUs, m.MemMB, m.Net,
			time.Since(m.Created).Round(time.Second), stopsIn(m))
	}
	return w.Flush()
}

func stopsIn(m *Meta) string {
	left := time.Duration(-1)
	if !m.Deadline.IsZero() {
		left = time.Until(m.Deadline)
	}
	if m.IdleSecs > 0 {
		if st, err := os.Stat(filepath.Join(vmDir(m.ID), "activity")); err == nil {
			idle := time.Duration(m.IdleSecs)*time.Second - time.Since(st.ModTime())
			if left < 0 || idle < left {
				left = idle
			}
		}
	}
	if left < 0 && m.Deadline.IsZero() && m.IdleSecs == 0 {
		return "never"
	}
	if left < 0 {
		left = 0
	}
	return left.Round(time.Second).String()
}

func cmdRm(args []string) error {
	f, err := parse(args, "", "all")
	if err != nil {
		return err
	}
	ids := f.pos
	if f.bools["all"] {
		vms, _ := listVMs()
		ids = nil
		for _, m := range vms {
			ids = append(ids, m.ID)
		}
	}
	if len(ids) == 0 && !f.bools["all"] {
		return fmt.Errorf("usage: veems rm <vm>... | --all")
	}
	var firstErr error
	for _, ref := range ids {
		id, err := resolve(ref)
		if err == nil {
			err = destroy(id)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "veems:", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func cmdTTL(args []string) error {
	f, err := parse(args, "ttl idle", "")
	if err != nil {
		return err
	}
	if len(f.pos) == 2 && len(f.vals["ttl"]) == 0 { // veems ttl <vm> 30m
		f.vals["ttl"] = []string{f.pos[1]}
		f.pos = f.pos[:1]
	}
	if len(f.pos) != 1 {
		return fmt.Errorf("usage: veems ttl <vm> [--ttl D] [--idle D]")
	}
	id, err := resolve(f.pos[0])
	if err != nil {
		return err
	}
	m, err := loadMeta(id)
	if err != nil {
		return err
	}
	if len(f.vals["ttl"]) > 0 {
		d, err := f.dur("ttl", 0)
		if err != nil {
			return err
		}
		m.Deadline = time.Time{}
		if d > 0 {
			m.Deadline = time.Now().UTC().Add(d)
		}
	}
	if len(f.vals["idle"]) > 0 {
		d, err := f.dur("idle", 0)
		if err != nil {
			return err
		}
		m.IdleSecs = int(d.Seconds())
	}
	touch(id)
	if err := saveMeta(m); err != nil {
		return err
	}
	fmt.Printf("%s stops in %s\n", id, stopsIn(m))
	return nil
}

func cmdScreenshot(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: veems screenshot <vm> out.png")
	}
	id, err := resolve(args[0])
	if err != nil {
		return err
	}
	tmp := filepath.Join(vmDir(id), "screen.png")
	touch(id)
	if _, err := qmp(vmDir(id), "screendump", map[string]any{"filename": tmp, "format": "png"}, 30*time.Second); err != nil {
		return err
	}
	b, err := os.ReadFile(tmp)
	if err != nil {
		return err
	}
	os.Remove(tmp)
	return os.WriteFile(args[1], b, 0o644)
}

func cmdBuild(args []string) error {
	f, err := parse(args, "iso", "snapshot desktop")
	if err != nil {
		return err
	}
	if len(f.pos) != 1 {
		return fmt.Errorf("usage: veems build <linux|windows|all> [--snapshot] [--iso PATH] [--desktop]")
	}
	targets := []string{f.pos[0]}
	if f.pos[0] == "all" {
		targets = []string{"linux", "windows"}
	}
	for _, t := range targets {
		p, err := profile(t)
		if err != nil {
			return err
		}
		if err := build(BuildOpts{OS: p.OS, Snapshot: f.bools["snapshot"], ISO: f.str("iso", ""), Desktop: f.bools["desktop"]}); err != nil {
			return fmt.Errorf("build %s: %w", p.OS, err)
		}
	}
	return nil
}

func cmdDoctor() error {
	ok := true
	check := func(name string, err error, fix string) {
		if err != nil {
			ok = false
			fmt.Printf("  FAIL  %-34s %v\n        fix: %s\n", name, err, fix)
		} else {
			fmt.Printf("  ok    %s\n", name)
		}
	}
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err == nil {
		f.Close()
	}
	check("/dev/kvm usable", err, "enable virtualization in firmware; add yourself to the kvm group")
	for _, b := range []struct{ bin, pkg string }{{qemuBin, "qemu-system-x86"}, {"qemu-img", "qemu-utils"},
		{"bwrap", "bubblewrap"}, {"pasta", "passt"}, {"xorriso", "xorriso"}} {
		_, err := exec.LookPath(b.bin)
		check(b.bin, err, "apt install "+b.pkg)
	}
	_, err = exec.LookPath("nft")
	if err != nil && exists("/usr/sbin/nft") {
		err = nil
	}
	check("nft", err, "apt install nftables")
	err = exec.Command("bwrap", "--unshare-all", "--ro-bind", "/", "/", "true").Run()
	check("unprivileged sandbox (bwrap)", err, "allow unprivileged user namespaces (kernel.unprivileged_userns_clone=1)")
	for _, os_ := range []string{"linux", "windows"} {
		var err error
		switch {
		case !exists(basePath(os_)):
			err = fmt.Errorf("not built")
		case readSnap(os_) == nil:
			err = fmt.Errorf("base image present, no snapshot (VMs cold boot)")
		case readSnap(os_).QEMU != qemuVersion():
			err = fmt.Errorf("snapshot is from another QEMU version")
		}
		check(os_+" image + snapshot", err, "veems build "+os_)
	}
	if b, err := os.ReadFile("/sys/kernel/mm/ksm/run"); err == nil && strings.TrimSpace(string(b)) != "1" {
		fmt.Println("  tip   KSM is off; `echo 1 | sudo tee /sys/kernel/mm/ksm/run` lets identical VMs share memory")
	}
	if !ok {
		return fmt.Errorf("some checks failed")
	}
	return nil
}
