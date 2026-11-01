package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const qemuBin = "qemu-system-x86_64"

// Spec is everything needed to start one QEMU process. Snapshot capture and
// restore must produce identical device lists, so both go through qemuArgs.
type Spec struct {
	OS       string
	Dir      string // holds sockets, logs, and (usually) the disk
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
		a = append(a, "-machine", "q35,accel=kvm", "-cpu", "host", "-device", "VGA,vgamem_mb=4")
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
	if s.Net {
		a = append(a, "-netdev", "user,id=net0,ipv6=off,net=10.0.2.0/24")
	} else {
		// A NIC wired to an empty hub: the guest sees a link, nothing is on it.
		a = append(a, "-netdev", "hubport,id=net0,hubid=0")
	}
	a = append(a,
		"-device", "virtio-net-pci,netdev=net0,mac=52:54:00:12:34:56",
		"-device", "virtio-serial-pci,id=vser0",
		"-chardev", "socket,id=qga0,path="+filepath.Join(s.Dir, "qga.sock")+",server=on,wait=off",
		"-device", "virtserialport,bus=vser0.0,chardev=qga0,name=org.qemu.guest_agent.0",
		"-chardev", "file,id=ser0,path="+filepath.Join(s.Dir, "console.log"),
		"-serial", "chardev:ser0",
		"-qmp", "unix:"+filepath.Join(s.Dir, "qmp.sock")+",server=on,wait=off",
	)
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
// directory (read-only) and its own VM directory, nothing else.
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
	bw = append(bw, "--bind", s.Dir, s.Dir, "--chdir", s.Dir)
	if s.Net {
		resolv := filepath.Join(s.Dir, "resolv.conf")
		if err := os.WriteFile(resolv, []byte("nameserver "+dnsForward+"\n"), 0o644); err != nil {
			return nil, err
		}
		bw = append(bw, "--share-net", "--ro-bind", resolv, "/etc/resolv.conf")
	}
	bw = append(bw, "--", qemu)
	bw = append(bw, qemuArgs(s)...)

	var argv []string
	if s.Net {
		argv = append([]string{"pasta", "--config-net", "--ipv4-only", "--no-map-gw", "--quiet",
			"--dns-forward", dnsForward, "-t", "none", "-u", "none", "-T", "none", "-U", "none",
			"--", "/bin/sh", "-c", netjailScript, "veems-netjail", firewallRules(), nftPath()}, bw...)
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

func qemuVersion() string {
	out, err := exec.Command(qemuBin, "--version").Output()
	if err != nil {
		return ""
	}
	return strings.SplitN(string(out), "\n", 2)[0]
}

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
		}
	}
}

// killOnSignal makes Ctrl-C or a kill of a foreground veems take its VM down too.
func killOnSignal(dir string) func() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		if _, ok := <-ch; ok {
			killVMProcs(dir)
			os.Exit(130)
		}
	}()
	return func() { signal.Stop(ch); close(ch) }
}
