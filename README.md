<img src="assets/icon.svg" width="64" height="64" alt="">

# veems

Fresh Linux and Windows VMs for AI coding agents, in one command.

```bash
VM=$(veems new linux)        # a brand-new VM, ready in about 0.3 seconds
veems exec $VM 'make test'   # run anything inside it
veems cp ./app $VM:app       # copy files in (and out)
veems rm $VM                 # gone for good
```

Agents can run untrusted code, install packages, or break the whole system,
and none of it touches your machine. Every VM starts clean, and when it closes
it is deleted, disk and all.

## Why

- **Fast.** VMs restore from a saved snapshot instead of booting: Linux is ready in ~0.3 s, Windows in ~0.5 s.
- **Sealed off.** Each VM is a real KVM virtual machine. No shared folders, no network by default, and `--net` gives internet access only, never your computer or your local network.
- **Light.** An idle Linux VM uses ~240 MB of RAM and under 1% of a CPU core.
- **Disposable.** VMs delete themselves after 20 minutes of no use, or 2 hours total, in case an agent forgets.
- **Agent-ready.** It's one CLI with no daemon. A skill file teaches Claude Code, Codex, OpenCode, oh-my-pi and pi to use it.

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
| Ready in | ~0.3 s | ~0.5 s |
| Each command | ~0.2 s | ~1.5 s |
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

## How it stays safe

1. **Hardware virtualization.** Each VM has its own kernel and memory.
2. **A minimal, sandboxed QEMU.** No USB passthrough, sound, or shared folders. QEMU runs with seccomp inside a bubblewrap jail that can't see your home folder.
3. **No network by default.** With `--net`, a firewall lets the VM reach the internet but blocks your computer, your LAN, and cloud metadata addresses.
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
- After upgrading QEMU, run `veems build <os> --snapshot` to refresh the saved snapshot.
- `veems doctor` checks that everything is set up.

## License

MIT
