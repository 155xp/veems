// veems: throwaway Linux and Windows VMs for agents, one command each.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

const version = "0.2.0"

const usage = `veems: fresh, sandboxed Linux/Windows VMs for agents.

Use linux unless the task needs Windows: it starts faster, runs commands
faster, and uses about a third of the memory.

  veems new linux                 start a fresh VM, print its id (~0.3 s)
  veems exec <vm> 'uname -a'      run a shell line (bash; PowerShell on Windows)
  veems exec <vm> -- python3 -V   run a program with arguments, no shell
  veems cp ./src <vm>:dest        copy a file or directory in
  veems cp <vm>:path ./out        copy a file or directory out
  veems rm <vm>                   stop it and delete everything
  veems run linux -- make test    new + exec + rm in one step

Every VM starts from the same clean snapshot and is deleted when it stops.
Nothing it does can reach your machine: no shared folders, no network unless
you pass --net, and even then only the internet, never this host or the LAN.

VMs remove themselves after 20 min with no veems commands (--idle) or 2 h in
total (--ttl), whichever comes first. Still run 'veems rm' when you are done.

Commands
  new <linux|windows> [--desktop] [--net] [--name N] [--ttl 2h] [--idle 20m]
      [--cpus N] [--mem MB] [--project NAME] [--no-pool] [--cold] [--json]
  exec <vm> [--cwd DIR] [--env K=V]... [--timeout 10m] [--stdin] [--gui] [--json]
      ('line' | -- prog args...)        --gui: start it on the desktop, return at once
  cp <src> <dst>         one side is <vm>:path; a relative or empty path is
                         under the work dir (/work, C:\work)
  run <linux|windows> [new flags] [--push LOCAL:REMOTE]... [--pull REMOTE:LOCAL]...
      ('line' | -- prog args...)
  ls [--json]            list running VMs
  rm <vm>... | --all     destroy VMs
  ttl <vm> [--ttl D] [--idle D]   change a running VM's limits (0 = no limit)
  build <linux|windows> [--desktop] [--snapshot] [--iso PATH]   one-time image build
  project add NAME <linux|windows> [--git URL [--ref REF] | --src DIR] [--setup 'line']
      [--dest DIR] [--net]       save a VM with the code checked out and set up;
                                 then 'new --project NAME' starts from it
  project ls | refresh NAME | rm NAME
  pool [<linux|windows> | --project NAME] N [--net] [--desktop]
                                 keep N VMs restored and waiting, so 'new' just
                                 claims one (0 removes the pool); 'pool' alone shows it
  doctor                 check this host
  skill [show|install]   print the agent skill, or install it for Claude
                         Code, Codex, OpenCode, oh-my-pi, and pi

Desktop (VMs started with --desktop; screenshot works on any VM)
  screenshot <vm> [out.png]       save the screen, prints "file WIDTHxHEIGHT"
  click <vm> X Y [--right] [--double]
  move <vm> X Y
  drag <vm> X1 Y1 X2 Y2
  scroll <vm> up|down|left|right [N]
  type <vm> 'text'                type ASCII text
  key <vm> COMBO...               e.g. enter, ctrl+l, alt+f4, ctrl+alt+delete
  view <vm> [--port N] [--no-open]   live, clickable view in your browser
  X Y are pixels from the top-left of the screenshot.

<vm> is an id like lnx-k3m9qa, a unique prefix of one, or a --name.
'exec' exits with the command's exit code, or 124 on timeout.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	os.Exit(run(os.Args[1], os.Args[2:]))
}

func run(cmd string, args []string) int {
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
	case "screenshot", "shot":
		err = cmdScreenshot(args)
	case "click", "move", "drag", "scroll":
		err = cmdPointer(cmd, args)
	case "type":
		err = cmdType(args)
	case "key", "keys":
		err = cmdKey(args)
	case "view":
		err = cmdView(args)
	case "build":
		err = cmdBuild(args)
	case "pool":
		err = cmdPool(args)
	case "project", "projects":
		err = cmdProject(args)
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
	case "_pool-fill":
		err = fillPools(false)
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
	return code
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

const newValued = "name ttl idle cpus mem project"
const newBools = "net cold json desktop no-pool"

func newOpts(f *flags, osName string) (NewOpts, error) {
	o := NewOpts{OS: osName, Desktop: f.bools["desktop"], Name: f.str("name", ""), Net: f.bools["net"], Cold: f.bools["cold"],
		Project: f.str("project", ""), NoPool: f.bools["no-pool"]}
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

func logf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "veems: "+format+"\n", a...)
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
	osName, ok := osArg(f, f.pos)
	if !ok || len(f.pos) > 1 {
		return fmt.Errorf("usage: veems new <linux|windows> [flags]  or  veems new --project NAME [flags]")
	}
	o, err := newOpts(f, osName)
	if err != nil {
		return err
	}
	start := time.Now()
	m, err := newVM(o)
	if err != nil {
		return err
	}
	if f.bools["json"] {
		printJSON(map[string]any{"id": m.ID, "os": m.OS, "net": m.Net, "from_snapshot": m.Snapshot, "from_pool": m.Pooled,
			"project": m.Project, "ready_ms": time.Since(start).Milliseconds(), "deadline": m.Deadline, "idle_secs": m.IdleSecs})
		return nil
	}
	fmt.Println(m.ID)
	return nil
}

// osArg is the os positional, which --project makes optional.
func osArg(f *flags, pos []string) (string, bool) {
	if len(pos) > 0 {
		return pos[0], true
	}
	return "", f.str("project", "") != ""
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
const execBools = "stdin json gui"

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
	if f.bools["gui"] {
		return 0, launchGUI(m, o)
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
	// With --project the os is optional, so the first word may be the command.
	pos, osName := f.pos, ""
	if len(pos) > 0 {
		if _, err := profile(pos[0], false); err == nil || f.str("project", "") == "" {
			osName, pos = pos[0], pos[1:]
		}
	}
	if osName == "" && f.str("project", "") == "" {
		return 2, fmt.Errorf("usage: veems run <linux|windows> [flags] ('line' | -- prog args...)")
	}
	no, err := newOpts(f, osName)
	if err != nil {
		return 2, err
	}
	eo, err := execTarget(f, pos)
	if err != nil {
		return 2, err
	}
	m, err := newVM(no)
	if err != nil {
		return 1, err
	}
	if f.bools["keep"] {
		fmt.Fprintln(os.Stderr, "veems: vm", m.ID, "kept running")
	} else {
		defer destroy(m.ID)
		defer onInterrupt(func() { destroy(m.ID) })()
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
	all, err := listVMs()
	if err != nil {
		return err
	}
	var vms []*Meta
	waiting := 0
	for _, m := range all {
		if m.Pool != "" {
			waiting++ // pooled, see `veems pool`
		} else {
			vms = append(vms, m)
		}
	}
	if f.bools["json"] {
		if vms == nil {
			vms = []*Meta{}
		}
		printJSON(vms)
		return nil
	}
	if waiting > 0 {
		defer fmt.Fprintf(os.Stderr, "(%d more waiting in the pool, see `veems pool`)\n", waiting)
	}
	if len(vms) == 0 {
		fmt.Fprintln(os.Stderr, "no vms running")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tOS\tCPUS\tMEM\tNET\tAGE\tSTOPS IN\tPROJECT")
	for _, m := range vms {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%dM\t%v\t%s\t%s\t%s\n", m.ID, m.Name, m.OS, m.CPUs, m.MemMB, m.Net,
			time.Since(m.Created).Round(time.Second), stopsIn(m), m.Project)
	}
	return w.Flush()
}

func stopsIn(m *Meta) string {
	t, ok := m.stopsAt()
	if !ok {
		return "never"
	}
	return max(time.Until(t), 0).Round(time.Second).String()
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
			if m.Pool == "" { // the pool is drained with `veems pool <os> 0`
				ids = append(ids, m.ID)
			}
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

// vmArg resolves the first positional argument to a VM.
func vmArg(pos []string, usage string) (*Meta, []string, error) {
	if len(pos) == 0 {
		return nil, nil, fmt.Errorf("usage: veems %s", usage)
	}
	id, err := resolve(pos[0])
	if err != nil {
		return nil, nil, err
	}
	m, err := loadMeta(id)
	return m, pos[1:], err
}

func cmdScreenshot(args []string) error {
	m, rest, err := vmArg(args, "screenshot <vm> [out.png]")
	if err != nil {
		return err
	}
	out := m.ID + ".png"
	if len(rest) > 0 {
		out = rest[0]
	}
	touch(m.ID)
	png, w, h, err := screenshot(m.ID)
	if err != nil {
		return err
	}
	if err := os.WriteFile(out, png, 0o644); err != nil {
		return err
	}
	fmt.Printf("%s %dx%d\n", out, w, h)
	return nil
}

func cmdPointer(cmd string, args []string) error {
	f, err := parse(args, "button", "double right")
	if err != nil {
		return err
	}
	usage := map[string]string{
		"click": "click <vm> X Y [--right] [--double]", "move": "move <vm> X Y",
		"drag": "drag <vm> X1 Y1 X2 Y2", "scroll": "scroll <vm> up|down|left|right [N] [--at X Y]",
	}[cmd]
	m, rest, err := vmArg(f.pos, usage)
	if err != nil {
		return err
	}
	p, err := newPointer(m)
	if err != nil {
		return err
	}
	defer p.q.Close()
	if cmd == "scroll" {
		if len(rest) < 1 {
			return fmt.Errorf("usage: veems %s", usage)
		}
		n := 3
		if len(rest) > 1 {
			if n, err = strconv.Atoi(rest[1]); err != nil {
				return err
			}
		}
		return p.scroll(rest[0], n)
	}
	want := map[string]int{"click": 2, "move": 2, "drag": 4}[cmd]
	if len(rest) != want {
		return fmt.Errorf("usage: veems %s  (pixels from the top-left of `veems screenshot`)", usage)
	}
	xy, err := atoiArgs(rest)
	if err != nil {
		return err
	}
	switch cmd {
	case "move":
		return p.move(xy[0], xy[1])
	case "drag":
		return p.drag(xy[0], xy[1], xy[2], xy[3])
	}
	button := f.str("button", "left")
	if f.bools["right"] {
		button = "right"
	}
	count := 1
	if f.bools["double"] {
		count = 2
	}
	return p.click(xy[0], xy[1], button, count)
}

func cmdType(args []string) error {
	m, rest, err := vmArg(args, "type <vm> 'text'")
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return fmt.Errorf("usage: veems type <vm> 'text'")
	}
	return typeText(m, rest[0])
}

func cmdKey(args []string) error {
	m, rest, err := vmArg(args, "key <vm> COMBO...  (e.g. enter, ctrl+l, alt+f4, ctrl+alt+delete)")
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return fmt.Errorf("usage: veems key <vm> COMBO...  (e.g. enter, ctrl+l, alt+f4)")
	}
	return pressKeys(m, rest)
}

func cmdView(args []string) error {
	f, err := parse(args, "port", "no-open")
	if err != nil {
		return err
	}
	m, _, err := vmArg(f.pos, "view <vm> [--port N] [--no-open]")
	if err != nil {
		return err
	}
	port, err := f.num("port")
	if err != nil {
		return err
	}
	return view(m, port, !f.bools["no-open"])
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
		p, err := profile(t, f.bools["desktop"])
		if err != nil {
			return err
		}
		if err := build(BuildOpts{OS: p.OS, Snapshot: f.bools["snapshot"], ISO: f.str("iso", ""), Desktop: f.bools["desktop"]}); err != nil {
			return fmt.Errorf("build %s: %w", p.Key, err)
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
	for _, key := range []string{"linux", "windows", "linux-desktop", "windows-desktop"} {
		p := profiles[key]
		fix := p.buildCmd()
		var err error
		switch {
		case !exists(basePath(key)) && p.Desktop:
			fmt.Printf("  --    %s not built (optional: %s)\n", key, fix)
			continue
		case !exists(basePath(key)):
			err = fmt.Errorf("not built")
		case readSnap(key) == nil:
			err = fmt.Errorf("base image present, no snapshot (VMs cold boot)")
		case readSnap(key).QEMU != qemuVersion():
			err = fmt.Errorf("snapshot is from another QEMU version")
		}
		check(key+" image + snapshot", err, fix)
	}
	for _, pj := range listProjects() {
		if why := pj.stale(); why != "" {
			fmt.Printf("  --    project %s is out of date (%s): veems project refresh %s\n", pj.Name, why, pj.Name)
		}
	}
	if b, err := os.ReadFile("/sys/kernel/mm/ksm/run"); err == nil && strings.TrimSpace(string(b)) != "1" {
		fmt.Println("  tip   KSM is off; `echo 1 | sudo tee /sys/kernel/mm/ksm/run` lets identical VMs share memory")
	}
	if !ok {
		return fmt.Errorf("some checks failed")
	}
	return nil
}

func cmdPool(args []string) error {
	f, err := parse(args, "project", "net desktop json")
	if err != nil {
		return err
	}
	if len(f.pos) == 0 {
		return showPools(f.bools["json"])
	}
	if f.pos[0] == "fill" && len(f.pos) == 1 {
		return fillPools(true)
	}
	usage := fmt.Errorf("usage: veems pool [<linux|windows> | --project NAME] N [--net] [--desktop]")
	osName, ok := osArg(f, f.pos[:len(f.pos)-1])
	if !ok || len(f.pos) > 2 {
		return usage
	}
	n, err := strconv.Atoi(f.pos[len(f.pos)-1])
	if err != nil || n < 0 {
		return usage
	}
	t := PoolTarget{Project: f.str("project", ""), Net: f.bools["net"], Size: n}
	if t.Project != "" {
		src, err := projectSource(t.Project)
		if err != nil && n > 0 {
			return err
		}
		if src != nil {
			t.Image = src.prof.Key
		}
	}
	if osName != "" {
		p, err := profile(osName, f.bools["desktop"])
		if err != nil {
			return err
		}
		if t.Project != "" && t.Image != "" && t.Image != p.Key {
			return fmt.Errorf("project %s is a %s project", t.Project, t.Image)
		}
		t.Image = p.Key
	}
	if err := setPool(t); err != nil {
		return err
	}
	return showPools(false)
}

const projectUsage = `usage:
  veems project add NAME <linux|windows> [--git URL [--ref REF] | --src DIR]
        [--setup 'line'] [--dest DIR] [--net] [--desktop] [--timeout 30m]
  veems project ls [--json]
  veems project refresh NAME     prepare it again: fresh checkout, setup, snapshot
  veems project rm NAME`

func cmdProject(args []string) error {
	if len(args) == 0 {
		args = []string{"ls"}
	}
	sub, args := args[0], args[1:]
	switch sub {
	case "add", "new", "create":
		f, err := parse(args, "git ref src setup dest timeout", "net desktop")
		if err != nil {
			return err
		}
		if len(f.pos) != 2 {
			return fmt.Errorf("%s", projectUsage)
		}
		o := PrepOpts{Name: f.pos[0], OS: f.pos[1], Desktop: f.bools["desktop"], Git: f.str("git", ""), Ref: f.str("ref", ""),
			Src: f.str("src", ""), Setup: f.str("setup", ""), Dest: f.str("dest", ""), Net: f.bools["net"]}
		if o.Git != "" && o.Src != "" {
			return fmt.Errorf("use --git or --src, not both")
		}
		if o.Git == "" && o.Src == "" && o.Setup == "" {
			return fmt.Errorf("nothing to prepare: give --git, --src, or --setup")
		}
		if o.Timeout, err = f.dur("timeout", 30*time.Minute); err != nil {
			return err
		}
		if _, err := loadProject(o.Name); err == nil {
			return fmt.Errorf("project %s exists: `veems project refresh %s` prepares it again, `veems project rm %s` removes it", o.Name, o.Name, o.Name)
		}
		_, err = prepare(o)
		return err
	case "refresh", "update":
		if len(args) != 1 {
			return fmt.Errorf("%s", projectUsage)
		}
		pj, err := loadProject(args[0])
		if err != nil {
			return err
		}
		o, err := pj.prepOpts()
		if err != nil {
			return err
		}
		if _, err := prepare(o); err != nil {
			return err
		}
		refill(poolKey(pj.Image, pj.Name, false))
		refill(poolKey(pj.Image, pj.Name, true))
		return nil
	case "rm", "remove", "delete":
		if len(args) != 1 {
			return fmt.Errorf("%s", projectUsage)
		}
		return removeProject(args[0])
	case "ls", "list":
		f, err := parse(args, "", "json")
		if err != nil {
			return err
		}
		ps := listProjects()
		if f.bools["json"] {
			if ps == nil {
				ps = []*Project{}
			}
			printJSON(ps)
			return nil
		}
		if len(ps) == 0 {
			fmt.Fprintln(os.Stderr, "no projects (veems project add NAME linux --git URL --setup 'make deps')")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tIMAGE\tFROM\tDEST\tPREPARED\tSTATUS")
		for _, pj := range ps {
			from := pj.Git
			if pj.Src != "" {
				from = pj.Src
			}
			status := "ready"
			if why := pj.stale(); why != "" {
				status = "out of date: " + why
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s ago (%.0fs)\t%s\n", pj.Name, pj.Image, from, pj.Dest,
				time.Since(pj.Prepared).Round(time.Minute), pj.PrepSecs, status)
		}
		return w.Flush()
	}
	return fmt.Errorf("%s", projectUsage)
}
