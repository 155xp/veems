package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

//go:embed guest
var guestFiles embed.FS

const (
	debianURL  = "https://cloud.debian.org/images/cloud/trixie/latest/debian-13-genericcloud-amd64.qcow2"
	windowsURL = "https://go.microsoft.com/fwlink/?linkid=2293312&clcid=0x409&culture=en-us&country=us"
	virtioURL  = "https://fedorapeople.org/groups/virt/virtio-win/direct-downloads/stable-virtio/virtio-win.iso"
	// The Windows guest is disposable and has no inbound network; this only
	// matters if someone attaches a display.
	windowsPassword = "veems-Passw0rd!"
)

type BuildOpts struct {
	OS       string
	Snapshot bool   // only recapture the snapshot
	ISO      string // windows: use this ISO instead of downloading the evaluation
	Desktop  bool   // windows: Desktop Experience instead of Server Core
}

func download(url, name string) (string, error) {
	dst := filepath.Join(cacheDir(), name)
	if exists(dst) {
		return dst, nil
	}
	os.MkdirAll(cacheDir(), 0o755)
	logf("downloading %s", name)
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("download %s: %s", url, resp.Status)
	}
	f, err := os.Create(dst + ".part")
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return "", err
	}
	f.Close()
	return dst, os.Rename(dst+".part", dst)
}

// makeISO packs files into a small ISO9660 image with the given volume label.
func makeISO(out, label string, files map[string][]byte) error {
	dir, err := os.MkdirTemp(filepath.Dir(out), "iso-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			return err
		}
	}
	o, err := exec.Command("xorriso", "-as", "mkisofs", "-quiet", "-V", label, "-J", "-r", "-o", out, dir).CombinedOutput()
	if err != nil {
		return fmt.Errorf("xorriso (install it with your package manager): %v: %s", err, o)
	}
	return nil
}

// runToPoweroff boots a build VM and waits until the guest powers itself off.
func runToPoweroff(s Spec, limit time.Duration) error {
	cmd, err := launchCmd(s)
	if err != nil {
		return err
	}
	logPath := filepath.Join(s.Dir, "qemu.log")
	lf, _ := os.Create(logPath)
	defer lf.Close()
	cmd.Stdout, cmd.Stderr = lf, lf
	if err := cmd.Start(); err != nil {
		return err
	}
	defer onInterrupt(func() { killVMProcs(s.Dir) })()
	defer killVMProcs(s.Dir)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	start := time.Now()
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			if err != nil {
				b, _ := os.ReadFile(logPath)
				return fmt.Errorf("build vm failed: %v\n%s", err, b)
			}
			return nil
		case <-tick.C:
			el := time.Since(start).Round(time.Second)
			logf("  still installing (%s elapsed; console: %s)", el, filepath.Join(s.Dir, "console.log"))
			if el > limit {
				cmd.Process.Kill()
				return fmt.Errorf("build did not finish within %s; see %s", limit, s.Dir)
			}
		}
	}
}

func build(o BuildOpts) error {
	p, err := profile(o.OS, o.Desktop)
	if err != nil {
		return err
	}
	dir := imageDir(p.Key)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	lock, err := lockFile(filepath.Join(dir, "build.lock"), false)
	if err != nil {
		return fmt.Errorf("another `veems build %s` is running", p.Key)
	}
	defer lock.Close()
	if !o.Snapshot || !exists(basePath(p.Key)) {
		start := time.Now()
		switch p.OS {
		case "linux":
			err = buildLinux(p)
		case "windows":
			err = buildWindows(p, o)
		}
		if err != nil {
			return err
		}
		logf("base image ready in %s", time.Since(start).Round(time.Second))
	}
	return captureSnapshot(p)
}

func buildLinux(p *Profile) error {
	img, err := download(debianURL, "debian-13-genericcloud-amd64.qcow2")
	if err != nil {
		return err
	}
	work := filepath.Join(imageDir(p.Key), "build")
	os.RemoveAll(work)
	os.MkdirAll(work, 0o755)
	disk := filepath.Join(work, "base.qcow2")
	logf("preparing disk")
	if o, err := exec.Command("qemu-img", "convert", "-O", "qcow2", img, disk).CombinedOutput(); err != nil {
		return fmt.Errorf("qemu-img convert: %v: %s", err, o)
	}
	if o, err := exec.Command("qemu-img", "resize", "-q", disk, fmt.Sprintf("%dG", p.DiskGB)).CombinedOutput(); err != nil {
		return fmt.Errorf("qemu-img resize: %v: %s", err, o)
	}
	userData, _ := guestFiles.ReadFile("guest/linux-user-data.yaml")
	seed := filepath.Join(work, "seed.iso")
	files := map[string][]byte{
		"user-data": userData,
		"meta-data": []byte("instance-id: veems-build\nlocal-hostname: veems\n"),
	}
	if p.Desktop {
		files["desktop.sh"], _ = guestFiles.ReadFile("guest/linux-desktop.sh")
	}
	if err := makeISO(seed, "CIDATA", files); err != nil {
		return err
	}
	logf("installing packages in a build VM (a few minutes)")
	if err := runToPoweroff(Spec{OS: p.OS, Desktop: p.Desktop, Dir: work, Disk: disk, CPUs: 4, MemMB: 2048, Net: true,
		CDROMs: []string{seed}, ROBinds: []string{cacheDir()}}, 30*time.Minute); err != nil {
		return err
	}
	return installBase(p, work, disk)
}

func buildWindows(p *Profile, o BuildOpts) error {
	iso := o.ISO
	var err error
	if iso == "" {
		logf("Windows Server 2025 evaluation ISO (~6 GB, downloaded once)")
		if iso, err = download(windowsURL, "windows-server-2025-eval.iso"); err != nil {
			return err
		}
	}
	if iso, err = filepath.Abs(iso); err != nil {
		return err
	}
	virtio, err := download(virtioURL, "virtio-win.iso")
	if err != nil {
		return err
	}
	work := filepath.Join(imageDir(p.Key), "build")
	os.RemoveAll(work)
	os.MkdirAll(work, 0o755)
	disk := filepath.Join(work, "base.qcow2")
	if err := qemuImgCreate(disk, "", p.DiskGB); err != nil {
		return err
	}
	index := "1" // Server Core
	if p.Desktop {
		index = "2" // Desktop Experience
	}
	ua, _ := guestFiles.ReadFile("guest/autounattend.xml")
	setup, _ := guestFiles.ReadFile("guest/veems-setup.ps1")
	desktop := "$false"
	if p.Desktop {
		desktop = "$true"
	}
	fill := strings.NewReplacer("{{IMAGE_INDEX}}", index, "{{PASSWORD}}", windowsPassword, "{{DESKTOP}}", desktop)
	uaText := fill.Replace(string(ua))
	setup = []byte(fill.Replace(string(setup)))
	unattend := filepath.Join(work, "unattend.iso")
	if err := makeISO(unattend, "VEEMS", map[string][]byte{
		"autounattend.xml": []byte(uaText),
		"veems-setup.ps1":  setup,
	}); err != nil {
		return err
	}
	logf("installing Windows unattended (15-30 minutes, runs once)")
	if err := runToPoweroff(Spec{OS: p.OS, Desktop: p.Desktop, Dir: work, Disk: disk, CPUs: 4, MemMB: 4096, Net: true,
		CDROMs:  []string{iso, virtio, unattend},
		ROBinds: []string{filepath.Dir(iso), cacheDir()}}, 90*time.Minute); err != nil {
		return err
	}
	return installBase(p, work, disk)
}

// installBase compacts the finished build disk into the read-only base image.
func installBase(p *Profile, work, disk string) error {
	logf("compacting base image")
	base := basePath(p.Key)
	for _, f := range []string{base, tmplDisk(p.Key), tmplState(p.Key), tmplMeta(p.Key)} {
		os.Chmod(f, 0o644)
		os.Remove(f)
	}
	if o, err := exec.Command("qemu-img", "convert", "-O", "qcow2", "-o", "cluster_size=65536", disk, base+".tmp").CombinedOutput(); err != nil {
		return fmt.Errorf("qemu-img convert: %v: %s", err, o)
	}
	if err := os.Rename(base+".tmp", base); err != nil {
		return err
	}
	os.Chmod(base, 0o444)
	return os.RemoveAll(work)
}

// captureSnapshot boots the base image with the runtime device layout, waits
// until it is idle, and saves RAM + device state. `veems new` restores that
// state onto a fresh copy-on-write disk instead of booting.
func captureSnapshot(p *Profile) error {
	start := time.Now()
	work := filepath.Join(imageDir(p.Key), "snap-work")
	os.RemoveAll(work)
	if err := os.MkdirAll(work, 0o755); err != nil {
		return err
	}
	defer os.RemoveAll(work)
	for _, f := range []string{tmplDisk(p.Key), tmplState(p.Key), tmplMeta(p.Key)} {
		os.Chmod(f, 0o644)
		os.Remove(f)
	}
	disk := filepath.Join(work, "snap.qcow2")
	if err := qemuImgCreate(disk, basePath(p.Key), 0); err != nil {
		return err
	}
	s := Spec{OS: p.OS, Desktop: p.Desktop, Dir: work, Disk: disk, CPUs: p.CPUs, MemMB: p.MemMB, ROBinds: []string{imageDir(p.Key)}}
	cmd, err := launchCmd(s)
	if err != nil {
		return err
	}
	lf, _ := os.Create(filepath.Join(work, "qemu.log"))
	defer lf.Close()
	cmd.Stdout, cmd.Stderr = lf, lf
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	defer onInterrupt(func() { killVMProcs(work) })()
	defer func() { killVMProcs(work); cmd.Process.Kill(); <-done }()

	logf("booting %s to snapshot it", p.Key)
	a := &agentConn{dir: work}
	defer a.close()
	deadline := time.Now().Add(p.Boot)
	for a.call("guest-ping", nil, nil, 2*time.Second) != nil {
		select {
		case <-done:
			b, _ := os.ReadFile(filepath.Join(work, "qemu.log"))
			return fmt.Errorf("snapshot vm exited:\n%s", b)
		default:
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("guest agent never came up; console log: %s", filepath.Join(work, "console.log"))
		}
		time.Sleep(200 * time.Millisecond)
	}
	logf("guest up after %s; letting it settle for %s", time.Since(start).Round(time.Second), p.Settle)
	time.Sleep(p.Settle)
	p.guest().beforeSnapshot(a)

	// Save with the agent connected, as it is after every restore (see
	// agentProxy), so clones answer at once.
	if err := a.call("guest-ping", nil, nil, 10*time.Second); err != nil {
		return fmt.Errorf("guest agent: %w", err)
	}
	state := filepath.Join(work, "snap.state")
	if err := saveState(work, state); err != nil {
		return err
	}
	qmp(work, "quit", nil, 5*time.Second)
	<-done

	if err := os.Rename(disk, tmplDisk(p.Key)); err != nil {
		return err
	}
	if err := os.Rename(state, tmplState(p.Key)); err != nil {
		return err
	}
	os.Chmod(tmplDisk(p.Key), 0o444)
	os.Chmod(tmplState(p.Key), 0o444)
	b, _ := json.Marshal(snapInfo{QEMU: qemuVersion(), CPUs: p.CPUs, MemMB: p.MemMB})
	if err := os.WriteFile(tmplMeta(p.Key), b, 0o644); err != nil {
		return err
	}
	st, _ := os.Stat(tmplState(p.Key))
	logf("snapshot saved (%d MB state) in %s", st.Size()>>20, time.Since(start).Round(time.Second))
	return nil
}
