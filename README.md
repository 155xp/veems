<img src="assets/icon.svg" width="64" height="64" alt="">

# veems

Fresh Linux and Windows VMs for AI coding agents, in one command.

```bash
VM=$(veems new linux)        # a brand-new VM, ready in about 0.2 seconds
veems exec $VM 'make test'   # run anything inside it
veems cp ./app $VM:app       # copy files in (and out)
veems rm $VM                 # gone for good
```

Agents can run untrusted code, install packages, or break the whole system,
and none of it touches your machine. Every VM starts clean, and when it closes
it is deleted, disk and all.

## Why

- **Fast.** VMs restore from a saved snapshot instead of booting: Linux answers in ~0.2 s, Windows in ~0.3 s, and each command after that takes ~20 ms on Linux.
- **Ready to work, not just booted.** A [project](#projects-start-with-the-work-done) saves a VM with your repo checked out and its setup already run, so a new VM starts there. An optional [warm pool](#warm-pool) keeps a few restored and waiting, so `veems new` takes a few milliseconds.
- **Sealed off.** Each VM is a real KVM virtual machine. No shared folders, no network by default, and `--net` gives internet access only, never your computer or your local network.
- **Light.** An idle Linux VM uses ~240 MB of RAM and under 1% of a CPU core.
- **Disposable.** VMs delete themselves after 20 minutes of no use, or 2 hours total, in case an agent forgets.
- **Agent-ready.** It's one CLI with no daemon (the pool is opt-in and refills itself with a short-lived background `veems`). A skill file teaches Claude Code, Codex, OpenCode, oh-my-pi and pi to use it.

## Install

You need a Linux machine with KVM.

```bash
sudo apt install qemu-system-x86 qemu-utils bubblewrap passt nftables xorriso golang-go
git clone https://github.com/155xp/veems && cd veems
./install.sh                # installs veems and the agent skill
veems build linux           # one time, ~3 min
veems build windows         # optional, one time, ~10 min
```

## The VMs

| | `linux` | `windows` |
|---|---|---|
| System | Debian 13 | Windows Server 2025 Core (evaluation) |
| Ready in | ~0.2 s | ~0.3 s |
| Each command | ~20 ms | ~1.3 s (PowerShell start-up) |
| Memory | 1 GB | 2 GB |

**Use Linux unless you need Windows.** It is faster and much lighter.

Add `--desktop` for a full graphical desktop that agents can see and click,
and you can watch live with `veems view`:

| | `linux --desktop` | `windows --desktop` |
|---|---|---|
| Desktop | Xfce with Chromium, 1280×800 | Windows Server 2025 desktop with Edge, 1024×768 |
| Ready in | ~0.6 s | ~0.8 s |
| Memory | 2 GB (~750 MB in use idle) | 4 GB (~1.5 GB in use idle) |

Desktop images are built separately (`veems build linux --desktop`) and are
only worth it when the task needs a screen.

## Commands

```bash
veems new linux [--net] [--desktop]     # start a VM, prints its id
veems exec <vm> 'command'               # run a shell command (PowerShell on Windows)
veems exec <vm> -- program arg1 arg2    # run a program directly
veems cp ./local <vm>:path              # copy into the VM
veems cp <vm>:path ./local              # copy out of the VM
veems run linux -- make test            # new + exec + rm in one step
veems ls                                # list running VMs
veems rm <vm>                           # delete a VM (or --all)
```

Desktop VMs:

```bash
veems view <vm>                 # watch and control the screen in your browser
veems screenshot <vm> shot.png  # save the screen (prints its size)
veems click <vm> 640 400        # click at x,y (--right, --double)
veems type <vm> 'hello'         # type text
veems key <vm> ctrl+l enter     # press keys
veems scroll <vm> down 5        # scroll
veems drag <vm> 10 10 300 300   # drag
veems exec <vm> --gui 'chromium https://example.com'   # open an app on the desktop
```

Run `veems help` for every flag.

## Projects: start with the work done

A fresh VM is clean, but it is not a workspace yet: the code still has to be
cloned and its dependencies installed. A project does that once and saves the
result, RAM and disk, as a snapshot of its own:

```bash
veems project add api linux --git https://github.com/you/api \
  --setup 'python3 -m venv .venv && .venv/bin/pip install -e . pytest && .venv/bin/pytest -q'
VM=$(veems new --project api)          # restored with /work/api checked out and set up
veems exec $VM --cwd /work/api '.venv/bin/pytest -q'
```

- `--git URL [--ref REF]` clones (shallow) into `/work/<repo>`; `--src DIR` copies a local directory instead; `--dest` picks another place.
- `--setup` runs once, in the checkout. The network is on while a project is prepared (needed for `git` and package installs) and is unplugged before the snapshot is saved: VMs from the project are offline unless you pass `--net`, like any other VM.
- End the setup with a run of your tests or build: the files they read are then already in memory in every VM made from the project.
- `veems project ls` shows projects and whether they are current. A project is tied to the image snapshot it was built on; after `veems build`, `veems project refresh NAME` prepares it again (a fresh checkout, setup, and snapshot). `veems project rm NAME` deletes it.

## Warm pool

Restoring takes ~0.2 s. If even that is too slow, keep VMs restored and waiting:

```bash
veems pool linux 2                     # keep two linux VMs ready
veems pool --project api 2             # or two VMs of a project
veems pool                             # show pools and their waiting VMs
veems pool linux 0                     # stop pooling, remove the waiting VMs
```

`veems new` then claims a waiting VM (a few ms) and a background `veems`
restores a replacement and exits. When the pool is empty, or there is none,
`veems new` restores a VM as usual; `--no-pool` skips the pool on purpose.

- Pools are per kind: image or project, and with or without `--net`. A VM that waited without network is never handed to a `--net` request, or the other way round.
- A waiting VM gets its `--name`, `--ttl` and `--idle` when it is claimed, and its clock is set then. Until then it has no time limit, holds its memory (~240 MB idle for Linux), and is left out of `veems ls` and `veems rm --all`.
- Waiting VMs remember which snapshot they came from. After an image rebuild or a project refresh, stale ones are removed instead of handed out, and the pool refills from the new snapshot; a pooled project whose image was rebuilt is prepared again first.
- There is no service to install. A pool refills after every `veems new` of its kind and whenever `veems pool` or `veems pool fill` runs. After a reboot it is empty until one of those happens: the first `veems new` restores as usual and starts the refill.

## Measured

`bench/bench.sh` times what a caller sees: `veems new` until it prints the id
(the guest has answered by then), and the first `veems exec` until it returns.
Medians of 10 runs on one 24-thread desktop (32 GB RAM, Debian 13, QEMU 10.0)
that was busy with other work at the time; expect some spread.

| Linux | `new` | first `exec true` | asked for a VM → command done |
|---|---|---|---|
| 0.1.0 | 355 ms | 206 ms | 561 ms |
| 0.2.0, old snapshot | 260 ms | 37 ms | 298 ms |
| 0.2.0, snapshot rebuilt by 0.2.0 | 191 ms | 17 ms | 218 ms |
| 0.2.0, claimed from a pool | 4 ms | 17 ms | 22 ms |

Until a repo's tests pass ([itsdangerous](https://github.com/pallets/itsdangerous),
297 tests, Python venv; the check is `pytest -q -x`):

| | until tests pass |
|---|---|
| 0.1.0: `new --net`, clone, venv + pip install, tests | 5.1 s |
| 0.2.0: `new --project` (prepared once, 7 s), tests | 0.71 s |
| 0.2.0: claimed from a project pool, tests | ~0.55 s |

The remaining time is the test run itself (~0.53 s warm). Also measured: a
64 MB `veems cp` in went from 3.3 s to 1.3 s and out from 2.7 s to 0.9 s, and
Windows from asking to a finished `Get-Date` went from 2.2 s to 1.7 s.

## How it stays safe

1. **Hardware virtualization.** Each VM has its own kernel and memory.
2. **A minimal, sandboxed QEMU.** No USB passthrough, sound, or shared folders. QEMU runs with seccomp inside a bubblewrap jail that sees only its disk and its sockets, not your home folder or the files veems keeps about the VM.
3. **No network by default.** With `--net`, a firewall lets the VM reach the internet but blocks your computer, your LAN, and cloud metadata addresses. If veems can't read your machine's networks to build that firewall, the VM doesn't start. The firewall is built from the routes the host has when the VM starts; networks behind a VPN that uses public addresses aren't covered.
4. **Throwaway disks.** Each VM writes to its own copy-on-write layer over a read-only image. Closing the VM deletes the layer.
5. **Careful file copies.** Files only move through `veems cp`, and anything copied out is checked so it can't land outside the folder you chose.
6. **Resource limits.** VMs run at low priority, return freed memory to the host, and won't start if your machine is low on RAM.

## For agents

`./install.sh` puts [`skills/veems/SKILL.md`](skills/veems/SKILL.md) where
Claude Code, Codex, OpenCode, oh-my-pi and pi look for skills. For any other
agent, paste the output of `veems skill show` into its instructions.

## Good to know

- Images live in `~/.local/share/veems` (Linux ~1.6 GB, Windows ~5 GB). Running VMs add only the space their own changes take, and give it back when they close.
- The Windows image is Microsoft's free 180-day evaluation. Rebuild it when it expires.
- After upgrading QEMU, run `veems build <os> --snapshot` to refresh the saved snapshot (and `veems project refresh` your projects). Snapshots saved by veems 0.1 still work; re-saving one with 0.2 takes ~70 ms off every restore.
- Projects live in `~/.local/share/veems/projects`; each is a few hundred MB (its RAM state plus the disk changes setup made). Only you can read them, since the checkout and the saved RAM may hold private code or credentials.
- `veems doctor` checks that everything is set up.

## License

MIT
