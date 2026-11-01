<img src="assets/icon.svg" width="64" height="64" alt="">

# veems

Throwaway Linux and Windows VMs for coding agents, one command each.

```bash
VM=$(veems new linux)          # fresh VM from a clean snapshot, ~0.3 s
veems exec $VM 'make test'     # run anything, exit code passes through
veems cp ./proj $VM:proj       # files in...
veems cp $VM:proj/out ./out    # ...and out
veems rm $VM                   # gone, nothing kept
```

Agents on this machine run tests, untrusted binaries, and destructive
experiments inside a VM instead of on the host. Each VM is a real KVM virtual
machine with its own kernel, started from a memory snapshot, so it is ready
before an agent finishes reading the id.

**Use the Linux VM unless the task really needs Windows.** It starts faster,
runs commands in ~0.2 s instead of 1 to 2 s, and uses about a third of the memory.

| | linux | windows |
|---|---|---|
| OS | Debian 13 (trixie), cloud kernel | Windows Server 2025 Standard Core (evaluation) |
| Size | 2 vCPU, 1 GB | 2 vCPU, 2 GB |
| `veems new` | ~0.3 s | ~0.5 s |
| `veems exec` | ~0.2 s (bash) | ~1.5 s (PowerShell) |
| Memory in use while idle | ~240 MB | ~780 MB |
| CPU while idle | ~0.8% of one core | ~1.6% of one core |
| Preinstalled | bash, python3, gcc/make, git, curl, jq | PowerShell 5.1, tar.exe, curl.exe |

## How sealed off is it?

A VM must never be able to hurt the host, even if the code inside wipes its
disk, forkbombs, or tries to escape. The layers, from the inside out:

1. **Hardware virtualization (KVM).** Separate kernel, separate memory. Root or SYSTEM inside the VM is still just a guest.
2. **Minimal QEMU.** No USB, sound, shared folders, clipboard, or display server. Only virtio disk, net, serial, and RNG devices. QEMU's seccomp sandbox is on (`-sandbox on`, no spawning, no privilege changes).
3. **QEMU runs in a bubblewrap jail.** New user, PID, IPC, mount, and network namespaces, all capabilities dropped. QEMU can see `/usr`, `/dev/kvm`, the read-only base images, and its own VM folder. It cannot see your home directory. Nothing runs as root.
4. **No network by default.** The NIC is plugged into an empty virtual hub, and the jail has no network namespace route out.
5. **`--net` means internet only.** QEMU runs inside a pasta network namespace with an nftables egress firewall that rejects this host, the LAN (all RFC 1918, CGNAT, and link-local ranges plus your local subnets), and cloud metadata addresses. DNS goes through one forwarded resolver address. The firewall is loaded before QEMU starts; if it fails to load, the VM doesn't start.
6. **Copy-on-write disks.** Every VM writes to its own overlay on top of a read-only base. Deleting the VM deletes the overlay. The guest can't change the base image or any other VM.
7. **File transfer goes through the guest agent only** (virtio-serial). Directories copied out of a VM are unpacked with a strict extractor: no absolute paths, no `..`, no writes through symlinks, no symlinks that point outside the destination, no device files or setuid bits.
8. **Resource limits.** CPU and RAM are fixed per VM, VM processes run at low CPU priority (nice 10), freed guest memory goes back to the host (virtio-balloon free page reporting), and `veems new` refuses to start a VM when the host would drop below 1 GB of free memory (`VEEMS_RESERVE_MB`).
9. **Fresh RNG per clone.** A VM generation ID device makes every restored clone reseed its random number generator, so clones never share key material.

## Things clean up after themselves

Agents forget. Every VM has two limits, and a small supervisor process
enforces them:

- **idle**: no `veems` command touched it for 20 minutes (`--idle`, `VEEMS_IDLE`). A running `exec` counts as activity.
- **ttl**: 2 hours since it started (`--ttl`, `VEEMS_TTL`).

A VM also goes away when its guest shuts down, on `veems rm`, or if QEMU
crashes. If the supervisor itself is killed, the next `veems` command finds
the leftovers and kills them. Change a running VM's limits with
`veems ttl <vm> 4h` or `veems ttl <vm> --idle 1h`.

## Install

Needs Linux with KVM (`/dev/kvm`), Go 1.22+ to build, and these packages:

```bash
sudo apt install qemu-system-x86 qemu-utils bubblewrap passt nftables xorriso golang-go
git clone https://github.com/155xp/veems && cd veems
./install.sh            # builds to ~/.local/bin/veems, installs the agent skill
veems doctor            # checks KVM, QEMU, sandbox, firewall
veems build linux       # once: ~3 min, downloads Debian's 330 MB cloud image
veems build windows     # once, optional: ~10 min, downloads 7 GB (eval ISO + virtio drivers)
```

Images and snapshots live in `~/.local/share/veems` (set `VEEMS_HOME` to
move them). Linux takes about 1.6 GB on disk and Windows about 5 GB, plus
the cached downloads, which you can delete afterwards.

`veems build <os> --snapshot` only recaptures the memory snapshot (about
10 s for Linux, 3 min for Windows). Rerun it after a QEMU upgrade. Until
then, `veems new` falls back to a cold boot and tells you so.

### Windows notes

- The image is Windows Server 2025 **Evaluation**, activated online during the build. The eval license lasts 180 days from the build; run `veems build windows` again after that. `--iso PATH` uses your own ISO, and `--desktop` installs the Desktop Experience edition instead of Server Core.
- Commands run as `NT AUTHORITY\SYSTEM`. Defender, Windows Update, the firewall, and error-report dialogs are off, so test binaries aren't quarantined and crashing programs don't hang.
- The Administrator password is `veems-Passw0rd!`. It only matters if you attach a display, because nothing can reach the VM over the network.

## Agent skill

`veems skill install` writes [`skills/veems/SKILL.md`](skills/veems/SKILL.md)
to `~/.agents/skills` (read by Codex, OpenCode, oh-my-pi, and pi) and to
`~/.claude/skills`, `~/.codex/skills`, and `~/.pi/agent/skills` when those
agents are installed. `veems skill show` prints it. For other agents, paste
it into their instructions file (AGENTS.md and the like).

## Commands

```
veems new <linux|windows> [--net] [--name N] [--ttl 2h] [--idle 20m] [--cpus N] [--mem MB] [--cold] [--json]
veems exec <vm> [--cwd DIR] [--env K=V]... [--timeout 10m] [--stdin] [--json] ('line' | -- prog args...)
veems cp <src> <dst>                      # one side is <vm>:path
veems run <linux|windows> [new flags] [--push LOCAL:REMOTE]... [--pull REMOTE:LOCAL]... [--keep] ('line' | -- prog args...)
veems ls [--json]
veems rm <vm>... | --all
veems ttl <vm> [--ttl D] [--idle D]
veems screenshot <vm> out.png
veems build <linux|windows|all> [--snapshot] [--iso PATH] [--desktop]
veems doctor
veems skill [show|install|uninstall]
```

`<vm>` is an id (`lnx-k3m9qa`), a unique prefix of one, or a `--name`.

## How it works

- **Build** (`veems build`): Linux starts from Debian's official cloud image. cloud-init installs the guest agent and tools, configures a static network address, turns off timers and daemons, and powers off. Windows installs unattended from the evaluation ISO, then a first-logon script adds the virtio drivers and the QEMU guest agent, quiets the system, activates the eval license, and powers off. Each result becomes a read-only `base.qcow2`.
- **Snapshot**: veems boots the base image with the exact runtime device layout, waits until it is idle, warms the shell, then saves RAM and device state (`snap.state`, about 185 MB for Linux) next to a frozen disk layer (`snap.qcow2`).
- **New**: veems creates a copy-on-write overlay on the snapshot disk and starts a supervisor. The supervisor launches the jailed QEMU with `-incoming file:snap.state` and resumes the VM. veems then sets the guest clock and the VM is ready.
- **Exec and copy** use the QEMU guest agent over virtio-serial. Each call opens a short connection under a per-VM lock, so any number of processes can share one VM safely.

Written in Go with no dependencies outside the standard library. The binary is about 6 MB.
