---
name: veems
description: Spin up a fresh, fully isolated Linux or Windows VM on this machine in about a second with the `veems` CLI, to run, build, or test code without touching the host. Use it for untrusted, destructive, or system-level work (installing packages, running unknown binaries, testing malware-ish or crashy programs, kernel/OS changes), for clean-machine tests, and for anything that needs Windows. Default to Linux; use Windows only when the task truly needs Windows.
---

# veems: throwaway VMs

`veems` gives you a brand-new VM from a clean snapshot in about a second.
Nothing inside it can reach this machine: no shared folders, no network
unless you ask, and even with `--net` the VM only reaches the internet, never
this host or the local network. Break it however you like; then delete it.

## Pick Linux unless you need Windows

- **linux** (Debian 13, 2 vCPU, 1 GB): ready in about 0.3 s, ~0.2 s per command. Has bash, python3, gcc/make, git, curl, jq.
- **windows** (Server 2025 Core, 2 vCPU, 2 GB): ready in about 0.5 s, but every command runs through PowerShell, which takes 1 to 2 s. Uses three times the memory.

Use `windows` **only** when the task specifically needs Windows (a Windows
binary, PowerShell, Windows paths or APIs, an .exe/.msi, a Windows-only bug).
Everything else goes on `linux`: it is lighter on this shared machine.

## Commands

```bash
VM=$(veems new linux)                 # prints only the id, e.g. lnx-k3m9qa
veems exec $VM 'cd /work && make test' # shell line: bash (PowerShell on Windows)
veems exec $VM -- python3 -c 'print(1)'  # program + args, no shell quoting
veems cp ./myproj $VM:proj            # copy in: lands in /work/proj (C:\work\proj)
veems cp $VM:proj/out.log ./out.log   # copy out: file or whole directory
veems rm $VM                          # always clean up when done
```

One-shot (create, copy in, run, copy out, delete):

```bash
veems run linux --push ./src:src --pull src/dist:./dist -- bash -c 'cd src && make'
```

Need the internet (apt, pip, npm, git clone)? Add `--net` to `new` or `run`:

```bash
VM=$(veems new linux --net)
veems exec $VM 'apt-get update -qq && apt-get install -y -qq nodejs'
```

## Desktop VMs (only when you need a screen)

Add `--desktop` when the task needs a GUI: testing a web app in a real
browser, a desktop app, or anything you must see and click. They use more
memory (Linux 2 GB, Windows 4 GB), so stay headless otherwise.

- Linux desktop: Xfce with Chromium, logged in as user `agent` on `DISPLAY=:0` (exec already sets DISPLAY, so `xdotool` and `wmctrl` work).
- Windows desktop: Windows Server 2025 with the full desktop, logged in as Administrator.

```bash
VM=$(veems new linux --desktop --net)
veems exec $VM --gui 'chromium https://example.com'  # opens on the desktop, returns at once
sleep 3; veems screenshot $VM shot.png               # prints "shot.png 1280x800"; look at it
veems click $VM 640 400                              # x y in screenshot pixels (--right, --double)
veems type $VM 'hello world'                         # ASCII text, US keyboard
veems key $VM ctrl+l enter                           # combos run in order
veems scroll $VM down 5
veems drag $VM 100 100 400 300
```

Take a fresh screenshot after each action to check its effect. Plain
`veems exec` (without `--gui`) still captures output, so you can launch and
check things from the shell too. The user can watch with `veems view $VM`.

## Details that matter

- `veems exec` exits with the command's exit code; stdout and stderr stay separate. `--json` gives `{exit_code, stdout, stderr}`.
- Default timeout per exec is 10 min (`--timeout 30m`, `--timeout 0` for none). Timeout exits 124 and kills the process tree.
- Output is returned when the command finishes (no live streaming). For long jobs, write to a log file and `veems cp` it out, or poll with `veems exec $VM 'tail -n 20 /work/log'`.
- Commands run as root (Linux) / SYSTEM (Windows). Default working dir: `/work` or `C:\work`. Use `--cwd DIR`, `--env K=V`, `--stdin` (pipes your stdin in).
- Relative or empty paths in `cp` are under the work dir: `$VM:` means `/work`. A trailing `/` on the destination puts the file inside that dir.
- Windows shell lines are PowerShell: `veems exec $VM 'Get-ChildItem; .\app.exe --help'`. For cmd syntax: `veems exec $VM -- cmd /c "dir /b"`.
- `veems ls` lists running VMs and when each will stop. Ids accept a unique prefix, or use `--name` on `new`.
- Each VM is independent; several agents can run many VMs at once, and several commands can hit one VM at once.

## Cleanup (automatic, but do it yourself)

Run `veems rm $VM` as soon as you are done. As a safety net, a VM deletes
itself after **20 min without any veems command** (`--idle`) or **2 h total**
(`--ttl`). A running `exec` counts as activity. If you need longer:
`veems new linux --ttl 6h --idle 1h`, or `veems ttl $VM 4h` on a running VM.
Nothing persists: every `veems new` starts from the same clean snapshot.

If `veems new` says there is not enough memory, other VMs are running:
remove yours (`veems ls`, `veems rm`), and never run more VMs than you need.

## If something is off

- `veems doctor` checks the host (KVM, QEMU, sandbox tools, images).
- "no linux image yet": the one-time image build hasn't run; tell the user to run `veems build linux` (or `windows`). Don't start that build yourself unless asked; the Windows one takes ~10 min and downloads 7 GB.
