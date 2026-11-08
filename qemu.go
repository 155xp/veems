package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const qemuBin = "qemu-system-x86_64"

// A QEMU process only reaches the run directory inside its VM (or build)
// directory, and its disk. Everything the host trusts (meta.json, markers,
// locks, logs) stays outside, so a compromised QEMU cannot edit it or plant
// symlinks for the host to write through.
func runDir(dir string) string     { return filepath.Join(dir, "run") }
func qgaSock(dir string) string    { return filepath.Join(runDir(dir), "qga.sock") }
func qmpSock(dir string) string    { return filepath.Join(runDir(dir), "qmp.sock") }
func consoleLog(dir string) string { return filepath.Join(runDir(dir), "console.log") }

// Spec is everything needed to start one QEMU process. Snapshot capture and
// restore must produce identical device lists, so both go through qemuArgs.
type Spec struct {
	OS       string
	Desktop  bool   // graphical image: bigger screen, USB tablet, VNC socket
	Dir      string // host-side directory; QEMU gets its run directory
	Disk     string
	CPUs     int
	MemMB    int
	Net      bool     // filtered internet; false = no network at all
	Incoming string   // snapshot state file to restore from
	CDROMs   []string // build only
	ROBinds  []string // extra read-only paths the sandbox needs
}

func qemuArgs(s Spec) []string {
	win := s.OS == "windows"
	a := []string{
		"-name", "veems,debug-threads=on",
		"-nodefaults", "-no-user-config",
		"-display", "none",
		"-msg", "timestamp=on",
		"-sandbox", "on,obsolete=deny,elevateprivileges=deny,spawn=deny,resourcecontrol=deny",
		"-smp", strconv.Itoa(s.CPUs),
		"-m", strconv.Itoa(s.MemMB),
		"-rtc", "base=utc",
	}
	if win {
		a = append(a,
			"-machine", "q35,accel=kvm,hpet=off",
			"-cpu", "host,hv_relaxed,hv_vapic,hv_spinlocks=0x1fff,hv_time,hv_vpindex,hv_synic,hv_stimer,hv_frequencies,hv_tlbflush,hv_ipi",
			"-global", "kvm-pit.lost_tick_policy=discard",
			"-device", "VGA,vgamem_mb=16",
		)
	} else {
		// GRUB's BIOS build resets the machine when there is no VGA at all.
		vga := "VGA,vgamem_mb=4"
		if s.Desktop {
			vga = "VGA,vgamem_mb=16"
		}
		a = append(a, "-machine", "q35,accel=kvm", "-cpu", "host", "-device", vga)
	}
	a = append(a,
		// New VM generation ID on every restore: guests reseed their RNG, so
		// clones of one snapshot never share random state.
		"-device", "vmgenid",
		"-object", "rng-random,id=rng0,filename=/dev/urandom",
		"-device", "virtio-rng-pci,rng=rng0",
		// Hands memory the guest frees back to the host.
		"-device", "virtio-balloon-pci,id=balloon0,free-page-reporting=on",
		"-drive", "if=none,id=disk0,format=qcow2,file="+s.Disk+",discard=unmap,detect-zeroes=unmap,cache=writeback,aio=threads",
	)
	if win {
		a = append(a, "-device", "ide-hd,bus=ide.0,drive=disk0,bootindex=1")
	} else {
		a = append(a, "-device", "virtio-blk-pci,drive=disk0,bootindex=1")
	}
	// The NIC always sits on a hub. With --net the internet joins the hub
	// through ports that can be removed (see prepare), which leaves exactly
	// the device state of a VM with no network.
	a = append(a, "-netdev", "hubport,id=net0,hubid=0")
	if s.Net {
		a = append(a, "-netdev", "user,id=uplink,ipv6=off,net=10.0.2.0/24",
			"-netdev", "hubport,id=uplinkport,hubid=0,netdev=uplink")
	}
	a = append(a,
		"-device", "virtio-net-pci,netdev=net0,mac=52:54:00:12:34:56",
		"-device", "virtio-serial-pci,id=vser0",
		"-chardev", "socket,id=qga0,path="+qgaSock(s.Dir)+",server=on,wait=off",
		"-device", "virtserialport,bus=vser0.0,chardev=qga0,name=org.qemu.guest_agent.0",
		"-chardev", "file,id=ser0,path="+consoleLog(s.Dir),
		"-serial", "chardev:ser0",
		"-qmp", "unix:"+qmpSock(s.Dir)+",server=on,wait=off",
	)
	if s.Desktop {
		// An absolute pointer so clicks land where they are aimed, and a VNC
		// server on a socket only this user can open (`veems view`).
		a = append(a, "-device", "qemu-xhci,id=usb", "-device", "usb-tablet,bus=usb.0",
			"-vnc", "unix:"+filepath.Join(runDir(s.Dir), "vnc.sock"))
	}
	for i, iso := range s.CDROMs {
		id := fmt.Sprintf("cd%d", i)
		a = append(a, "-drive", "if=none,media=cdrom,readonly=on,id="+id+",file="+iso,
			"-device", fmt.Sprintf("ide-cd,bus=ide.%d,drive=%s,bootindex=%d", i+1, id, i+2))
	}
	if s.Incoming != "" {
		a = append(a, "-incoming", "file:"+s.Incoming)
	}
	return a
}

// launchCmd wraps QEMU in the sandbox:
//
//	no network:  bwrap (empty net namespace) -> qemu
//	network:     pasta (own net namespace, NAT to the internet) -> veems _netjail
//	             (firewall: no host, no LAN) -> bwrap -> qemu
//
// bwrap also hides the host filesystem: QEMU sees /usr, /dev/kvm, the image
// directory (read-only), its disk, and the run directory, nothing else.
func launchCmd(s Spec) (*exec.Cmd, error) {
	qemu, err := exec.LookPath(qemuBin)
	if err != nil {
		return nil, fmt.Errorf("qemu not found: install qemu-system-x86 (see `veems doctor`)")
	}
	bw := []string{"bwrap",
		"--unshare-all", "--die-with-parent", "--new-session", "--cap-drop", "ALL",
		"--proc", "/proc", "--dev", "/dev", "--dev-bind", "/dev/kvm", "/dev/kvm",
		"--tmpfs", "/tmp", "--ro-bind-try", "/etc/ld.so.cache", "/etc/ld.so.cache",
	}
	for _, d := range []string{"/usr", "/lib", "/lib64", "/bin", "/sbin"} {
		if t, err := os.Readlink(d); err == nil {
			bw = append(bw, "--symlink", t, d)
		} else if exists(d) {
			bw = append(bw, "--ro-bind", d, d)
		}
	}
	for _, p := range s.ROBinds {
		bw = append(bw, "--ro-bind", p, p)
	}
	run := runDir(s.Dir)
	if err := os.MkdirAll(run, 0o700); err != nil {
		return nil, err
	}
	for _, iso := range s.CDROMs {
		bw = append(bw, "--ro-bind", iso, iso)
	}
	bw = append(bw, "--bind", s.Disk, s.Disk, "--bind", run, run, "--chdir", run)
	if s.Net {
		resolv := filepath.Join(run, "resolv.conf")
		if err := os.WriteFile(resolv, []byte("nameserver "+dnsForward+"\n"), 0o644); err != nil {
			return nil, err
		}
		bw = append(bw, "--share-net", "--ro-bind", resolv, "/etc/resolv.conf")
	}
	bw = append(bw, "--", qemu)
	bw = append(bw, qemuArgs(s)...)

	var argv []string
	if s.Net {
		rules, err := firewallRules()
		if err != nil {
			return nil, err
		}
		argv = append([]string{"pasta", "--config-net", "--ipv4-only", "--no-map-gw", "--quiet",
			"--dns-forward", dnsForward, "-t", "none", "-u", "none", "-T", "none", "-U", "none",
			"--", "/bin/sh", "-c", netjailScript, "veems-netjail", rules, nftPath()}, bw...)
	} else {
		argv = bw
	}
	for _, bin := range []string{argv[0], "bwrap"} {
		if _, err := exec.LookPath(bin); err != nil {
			return nil, fmt.Errorf("%s not found (see `veems doctor`)", bin)
		}
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	return cmd, nil
}

// qemuVersion is cached on disk by the binary's size and mtime: running
// `qemu --version` costs ~20 ms, and `veems new` asks every time.
var qemuVersion = sync.OnceValue(func() string {
	bin, err := exec.LookPath(qemuBin)
	if err != nil {
		return ""
	}
	cache := filepath.Join(cacheDir(), "qemu-version")
	key := stamp(bin) + " " + bin + "\n"
	if b, err := os.ReadFile(cache); err == nil && strings.HasPrefix(string(b), key) {
		return strings.TrimPrefix(string(b), key)
	}
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		return ""
	}
	v := strings.SplitN(string(out), "\n", 2)[0]
	if os.MkdirAll(cacheDir(), 0o755) == nil {
		os.WriteFile(cache+".tmp", []byte(key+v), 0o644)
		os.Rename(cache+".tmp", cache)
	}
	return v
})

func qemuImgCreate(path, backing string, sizeGB int) error {
	args := []string{"create", "-q", "-f", "qcow2", "-o", "cluster_size=65536,lazy_refcounts=on"}
	if backing != "" {
		args = append(args, "-F", "qcow2", "-b", backing)
	}
	args = append(args, path)
	if sizeGB > 0 {
		args = append(args, fmt.Sprintf("%dG", sizeGB))
	}
	out, err := exec.Command("qemu-img", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("qemu-img create: %v: %s", err, out)
	}
	return nil
}

// killVMProcs SIGKILLs every sandbox process (pasta, bwrap, QEMU) whose
// command line names dir. Parent-death signals do not survive pasta and
// bwrap's namespace setup, so cleanup never relies on them.
func killVMProcs(dir string) {
	var killed []string
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		comm, _ := os.ReadFile("/proc/" + e.Name() + "/comm")
		switch strings.TrimSpace(string(comm)) {
		case "qemu-system-x86", "bwrap", "pasta", "pasta.avx2":
		default:
			continue
		}
		cl, _ := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if strings.Contains(string(cl), dir+"/") {
			syscall.Kill(pid, syscall.SIGKILL)
			killed = append(killed, e.Name())
		}
	}
	// wait (up to 5 s) until their memory is back, so a following `new` sees it
	for _, pid := range killed {
		for i := 0; i < 250 && running(pid); i++ {
			time.Sleep(20 * time.Millisecond)
		}
	}
}

// running reports whether a process exists and is not yet a zombie.
func running(pid string) bool {
	st, err := os.ReadFile("/proc/" + pid + "/stat")
	if err != nil {
		return false
	}
	_, after, _ := strings.Cut(string(st), ") ")
	return !strings.HasPrefix(after, "Z")
}

var interrupts struct {
	sync.Mutex
	once     sync.Once
	cleanups []*func()
}

// onInterrupt runs cleanup if this process is interrupted or told to stop
// before the returned cancel is called. Cleanups run newest first, then the
// process exits.
func onInterrupt(cleanup func()) (cancel func()) {
	interrupts.once.Do(func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
		go func() {
			<-ch
			interrupts.Lock()
			for i := len(interrupts.cleanups) - 1; i >= 0; i-- {
				(*interrupts.cleanups[i])()
			}
			os.Exit(130)
		}()
	})
	interrupts.Lock()
	defer interrupts.Unlock()
	p := &cleanup
	interrupts.cleanups = append(interrupts.cleanups, p)
	return func() {
		interrupts.Lock()
		defer interrupts.Unlock()
		for i, c := range interrupts.cleanups {
			if c == p {
				interrupts.cleanups = append(interrupts.cleanups[:i], interrupts.cleanups[i+1:]...)
				break
			}
		}
	}
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
