# cloister — Multi-Account Claude Code Isolation CLI

## Goal

A Go CLI that lets any Mac developer run multiple isolated Claude Code accounts in Colima VMs with zero knowledge of Colima, SSH tunnels, or VM internals. Install via Homebrew, create a profile, and enter it — everything else is automatic.

## Target User

Mac developers who want isolated Claude Code accounts for different organizations or clients. They've never heard of Colima and shouldn't need to care about it. The tool should be as simple as `cloister create work && cloister work`.

## Isolation Model

**cloister provides credential and session isolation, not workspace isolation.** All profiles share `~/code` (read-write) and `~/.ssh` (read-only) intentionally — developers work on the same codebase from different org accounts, and need the same SSH keys for Git access.

What is **isolated** per profile:
- `~/.claude/` — org credentials, CLAUDE.md, conversation history, settings
- Claude Code auth tokens — each profile has its own `claude login` session

What is **intentionally shared** across profiles:
- `~/code/` — development workspace (read-write)
- `~/.ssh/` — SSH keys (read-only, enforced via post-boot `mount -o remount,ro`)
- `~/Downloads/` — file access (read-only, enforced via post-boot `mount -o remount,ro`)
- `~/.claude/plugins/`, `skills/`, `agents/` — shared extensions (read-write)

GPG signing keys are **never** copied into the VM (see GPG Commit Signing). The host's gpg-agent extra-socket is reverse-forwarded over SSH so signing transits the host while keys stay on macOS.

**Read-only enforcement:** Colima's virtiofs with `vz` does not support per-mount write protection. Read-only semantics are enforced inside the VM via `mount -o remount,ro` in a boot script for `~/.ssh`, `~/.gnupg`, and `~/Downloads`. This prevents accidental writes while keeping the mount simple.

## Architecture

```
cloister (Go binary, installed via Homebrew)
├── VM Lifecycle (create, start, stop, delete, rebuild)
│   └── Colima (implementation detail, never exposed to user)
├── Profile Manager
│   ├── Interactive wizard with defaults-first prompt
│   ├── Non-interactive mode via flags
│   ├── Config file: ~/.cloister/config.yaml
│   └── AI-friendly: --json output, --list-options discovery
├── Memory Manager
│   ├── Auto-calculated budget: (system RAM - 10GB), min 4GB
│   ├── Soft limit with prompt (interactive)
│   ├── Error with suggestion (non-interactive)
│   └── Idle tracking via last cloister entry timestamp
├── Tunnel Manager
│   ├── Auto-detect host services on profile entry
│   ├── Built-in: clipboard (cc-clip :18339), 1Password (op-forward :18340), gpg-forward (Unix socket), audio (PulseAudio :4713), ollama (:11434)
│   ├── Guided install suggestions for missing services
│   ├── Custom tunnels via config.yaml
│   ├── Implementation: SSH reverse port forwarding (-R) via dedicated connection (ControlMaster=no)
│   └── Health check display in status
├── Provisioning Engine
│   ├── Base install (always): git, Node LTS, pnpm, Claude Code, gpg client, tunnel shims
│   ├── Composable stacks: web, cloud, dotnet, python, go, rust, data
│   ├── Stack version configuration via flags or config
│   └── Custom provision scripts: ~/.cloister/provision.sh or provision-<profile>.sh
├── Backup/Restore
│   ├── Session data preservation: projects, tasks, file-history, settings
│   ├── GPG key material excluded from backups (security)
│   ├── 5 retained backups per profile
│   └── Rebuild cycle: backup → destroy → provision → restore
├── Terminal Integration
│   ├── iTerm2: background color + tab/window title via OSC sequences
│   ├── Tab title: "✱ Claude Code [profile]"
│   └── Fallback: profile banner for non-iTerm terminals
└── Self-Update (GitHub Releases, Homebrew tap)
```

## Key Principles

- **Colima is invisible**: never mentioned in user-facing output, help text, or errors
- **Zero config to start**: `cloister create work --defaults` provisions a fully working VM
- **Interactive by default, non-interactive via flags**: every prompt has a flag equivalent
- **AI-friendly**: `--json` output, `--list-options` for discovery, descriptive errors with fix suggestions, AI hints in help text
- **Convention over configuration**: sensible defaults for everything, YAML config for customization
- **Operations are idempotent**: `cloister create` on an existing profile is a no-op with a message, `cloister stop` on a stopped VM succeeds silently

## Minimum Requirements

- macOS 13+ (Ventura) — required for Apple Virtualization Framework
- Apple Silicon or Intel Mac
- 16GB RAM recommended (minimum 12GB — budget formula yields 2GB, enough for one 2GB profile)
- Homebrew installed
- Colima 0.8+ (auto-installed if missing via `brew install colima`)

## CLI Command Surface

```
cloister create <profile>          Create a new isolated VM profile
cloister <profile>                 Enter a profile (start VM if needed)
cloister stop [profile|all]        Stop VM(s) to free memory
cloister status                    Show all profiles, running state, memory, tunnels
cloister delete <profile>          Destroy a VM and its isolated data
cloister update [profile|all]      Update Claude Code and system packages in VM(s)
cloister backup [profile|all]      Back up session data
cloister restore <profile>         Restore from backup
cloister rebuild <profile>         Backup → destroy → provision → restore
cloister setup <service>           Guided install for tunneled services
cloister add-stack <profile> <s>   Add a provisioning stack to an existing profile
cloister config                    Open config file in $EDITOR (validates YAML on save)
cloister version                   Print version
cloister self-update               Update cloister itself
```

## Profile Entry Mechanism

`cloister <profile>` is the primary user interaction. Here's what it does:

1. **Start VM** if not running (`colima start -p cloister-<profile>` with output suppressed)
2. **Check memory budget** — prompt to stop idle VMs if exceeded
3. **Start tunnels** — SSH reverse port forwards for each detected host service (dedicated connections with `ControlMaster=no`)
4. **Set terminal identity** — iTerm2 background color + tab title, or fallback banner
5. **SSH into VM** — `colima ssh -p cloister-<profile>` which drops user into bash
6. On first entry after creation, print: `Run 'claude login' to authenticate with your Claude account`

The user lands in a bash shell inside the VM, at their configured `start_dir`. From there they run `claude` as normal.

## Profile Creation Flow

### Interactive mode

```
$ cloister create work

Creating profile "work"...
Use defaults? (4GB RAM, ~/code, auto color) [Y/n]: n

Memory allocation (GB) [4]: 6
Starting directory [~/code]: ~/code/my-project
Background color (hex, no #) [auto]: 0a1628
Provisioning stacks (web,cloud,dotnet,python,go,rust,data) [none]: web,cloud
Enable GPG commit signing? [y/N]: y

Profile "work" created. Enter with: cloister work
```

### Non-interactive mode

```
cloister create work --defaults
cloister create work --memory 6 --start-dir ~/code/my-project --color 0a1628 --stack web,cloud --gpg-signing
```

### AI-friendly discovery

```
$ cloister create --list-options --json
{
  "options": {
    "memory": {"type": "int", "default": 4, "unit": "GB", "hint": "RAM allocation for the VM"},
    "start_dir": {"type": "path", "default": "~/code", "hint": "Directory to cd into on entry. Must be under a mounted path (~/code, ~/Downloads)"},
    "color": {"type": "hex", "default": "auto", "hint": "iTerm2 background color (6-char hex, no #)"},
    "stacks": {"type": "list", "values": ["web", "cloud", "dotnet", "python", "go", "rust", "data"], "hint": "Provisioning bundles to install"},
    "gpg_signing": {"type": "bool", "default": false, "hint": "Enable GPG commit signing in VM"},
    "disk": {"type": "int", "default": 40, "unit": "GB", "hint": "VM disk size (advanced, not in wizard)"},
    "cpu": {"type": "int", "default": 4, "hint": "CPU cores (advanced, not in wizard)"},
    "dotnet_version": {"type": "string", "default": "10", "hint": ".NET SDK major version"},
    "node_version": {"type": "string", "default": "lts", "hint": "Node.js version (lts, 22, 20, latest)"},
    "python_version": {"type": "string", "default": "latest", "hint": "Python version via pyenv"},
    "go_version": {"type": "string", "default": "latest", "hint": "Go version (e.g., 1.24)"},
    "rust_version": {"type": "string", "default": "stable", "hint": "Rust toolchain (stable, nightly, 1.83)"},
    "terraform_version": {"type": "string", "default": "latest", "hint": "Terraform version"}
  }
}
```

**Validation:** `start_dir` is validated against mounted paths at creation time. If the user sets `start_dir: ~/Documents/something` and `~/Documents` is not mounted, creation fails with a clear error explaining which paths are available.

## Config File

Location: `~/.cloister/config.yaml`

```yaml
memory_budget: 16  # GB total for VMs, auto-calculated if omitted

profiles:
  work:
    memory: 6
    start_dir: ~/code/my-project
    color: "0a1628"
    stacks: [web, cloud]
    gpg_signing: true
    dotnet_version: "10"
  personal:
    memory: 4
    color: "000000"
    stacks: [web]

tunnels:
  - name: my-service
    host_port: 9000
    vm_port: 9000
    health_check: http://127.0.0.1:9000/health
```

`cloister config` opens this file in `$EDITOR` and validates YAML syntax on save.

## Memory Management

### Budget calculation

Default budget: `max((total system RAM - 10GB), 4)` rounded down. Ensures at least one profile can run on a 12GB Mac. User can override via `memory_budget` in config.

If the default profile memory (4GB) exceeds the calculated budget on a low-RAM machine, `cloister create` adjusts the default down and informs the user.

### Idle tracking

Idle time is tracked by recording a timestamp in `~/.cloister/state/<profile>.last_entry` on each `cloister <profile>` invocation. This avoids polling overhead — the timestamp is compared to the current time when the memory manager needs to suggest eviction candidates.

### Enforcement (interactive)

```
$ cloister work

Starting "work" (4GB)...
⚠ Memory budget exceeded: 18GB used of 16GB budget
  Running VMs:
    personal   4GB  (idle 3h)
    project-b   6GB  (idle 45m)
    default    4GB  (active)

  Stop "personal" to free 4GB? [Y/n]:
```

### Enforcement (non-interactive)

```
$ cloister work --non-interactive
Error: memory budget exceeded (18GB/16GB)
  Suggestion: cloister stop personal  # idle 3h, frees 4GB
  Exit code: 1
```

### Status view

```
$ cloister status

PROFILE      STATE     MEMORY   IDLE     STACKS
personal     running   4GB      3h       web
project-b     running   6GB      45m      web,cloud
default      running   4GB      active   web,dotnet
work         stopped   4GB      —        web,cloud

Budget: 14GB / 16GB used
Tunnels: clipboard ✓  op-forward ✓  audio ✗ (not installed)
```

### Concurrency

Multiple terminal windows can enter the same profile simultaneously — they share the same VM. `cloister stop` while another terminal is inside the profile will warn: "Profile 'work' has active sessions. Stop anyway? [y/N]". In non-interactive mode, `--force` overrides the check.

## Tunnel Manager

### Auto-discovery on profile entry

```
$ cloister work

Starting "work"...
Tunnels:
  ✓ clipboard (cc-clip on :18339)
  ✓ 1Password (op-forward on :18340)
  ✗ audio (PulseAudio not detected)
    → For /voice support: brew install pulseaudio
    → Then: cloister setup audio

Entering work...
```

### Implementation

All tunnels use SSH reverse port forwarding (`ssh -fN -R <port>:127.0.0.1:<port>`). Each tunnel uses a dedicated SSH connection with `ControlMaster=no` to avoid the multiplexing issue where `RemoteForward` is only established on the first connection. PID files are stored at `~/.cloister/state/tunnel-<service>-<profile>.pid` for lifecycle management.

### Built-in tunnels

| Service | Port | Health check | Install command |
|---------|------|-------------|-----------------|
| Clipboard (cc-clip) | 18339 | `http://127.0.0.1:18339/health` | `brew install ShunmeiCho/tap/cc-clip` |
| 1Password (op-forward) | 18340 | `http://127.0.0.1:18340/health` | `brew install ekovshilovsky/tap/op-forward && op-forward service install` |
| Audio (PulseAudio) | 4713 | TCP connect check | `brew install pulseaudio && cloister setup audio` |

### Tunnel shim deployment

Tunnel shims are shell scripts installed in `~/.local/bin/` inside each VM during provisioning. They intercept commands (e.g., `op`, `xclip`) and forward them to the host service via the SSH tunnel. Shims are deployed:
- During initial provisioning (for services detected at that time)
- On profile entry, if a new host service is detected that wasn't present during provisioning (cloister copies the shim via `colima ssh`)

### Guided setup

```
$ cloister setup op-forward
Installing op-forward for 1Password CLI forwarding...
  brew install ekovshilovsky/tap/op-forward
  op-forward service install
  ✓ Daemon running on :18340
  ✓ Tunnel shim will be deployed to VMs automatically

$ cloister setup audio
Configuring PulseAudio for /voice support...
  Detecting microphone...
    1: MacBook Pro Microphone
    2: Microsoft Teams Audio
  Select default microphone [1]: 1
  ✓ Audio forwarding configured
```

### Custom tunnels

```yaml
tunnels:
  - name: my-service
    host_port: 9000
    vm_port: 9000  # defaults to host_port if omitted
    health_check: http://127.0.0.1:9000/health  # optional
```

Custom tunnels get the same SSH reverse-forward treatment — auto-started on profile entry, shown in status.

## Provisioning

### Base (always installed)

- git, git-lfs, curl, wget, jq, direnv
- NVM + Node LTS + pnpm
- Claude Code (native installer)
- gpg client (no in-VM agent: signing is forwarded to the host gpg-agent)
- Tunnel shims (cc-clip, op-forward, op — auto-deployed if host services detected)
- ALSA/PulseAudio client config (if audio tunnel available)

### Stacks

| Stack | What it installs | Version flag | Default |
|-------|-----------------|-------------|---------|
| `web` | Playwright + Chromium, GitHub CLI, Vercel CLI | `--node-version` | Node LTS |
| `cloud` | AWS CLI v2, gcloud, Azure CLI, Terraform, tflint, tfsec | `--terraform-version` | latest |
| `dotnet` | .NET SDK, mssql-tools, PostgreSQL client | `--dotnet-version` | 10 (LTS) |
| `python` | Python (via pyenv), pip, venv | `--python-version` | latest stable |
| `go` | Go (official tarball) | `--go-version` | latest stable |
| `rust` | Rust (via rustup), cargo | `--rust-version` | stable |
| `data` | mongosh, PostgreSQL client, jq | — | — |
| `ollama` | Ollama CLI (server disabled — inference via host tunnel) | — | — |

Stacks are composable: `--stack web,cloud,python,ollama`

### Adding stacks after creation

`cloister add-stack <profile> <stack>` requires the VM to be running. If stopped, it starts it first. The stack provisioning script runs via SSH. If provisioning fails, the error is reported but the VM is left running (not rolled back) — the user can retry or fix manually.

```
$ cloister add-stack work dotnet
Starting "work" if needed...
Installing "dotnet" stack in "work"...
  ✓ .NET 10 SDK
  ✓ mssql-tools
  ✓ PostgreSQL client
Done. Stack added to profile config.
```

### Update scope

`cloister update [profile|all]` does:
1. `claude install latest` — updates Claude Code to latest version
2. `sudo apt-get update && sudo apt-get upgrade -y` — system security patches
3. Re-runs stack provisioning scripts in "update mode" (installs latest versions of stack-managed tools)

It does NOT change Node/Python/Go/Rust/Terraform versions — those are pinned per profile config. Use `cloister config` to change version pins, then `cloister rebuild` for a clean re-provision.

### Custom provisioning hook

After stacks run, `cloister` checks for:
- `~/.cloister/provision.sh` — runs for all profiles
- `~/.cloister/provision-<profile>.sh` — runs for specific profile

## GPG Commit Signing

Opt-in via `--gpg-signing` flag at creation or `gpg_signing: true` in config.

cloister forwards the host's gpg-agent into the VM rather than shipping any
private key material. Signing requests originate inside the VM, traverse a
reverse-forwarded Unix socket to the host gpg-agent's restricted *extra*
socket, and are answered by macOS pinentry — passphrases never enter the VM
and are cached by the macOS Keychain.

### Host preflight

`cloister setup gpg-forward` is a one-time host-side step that:

1. Locates or installs `pinentry-mac` via Homebrew.
2. Writes `pinentry-program /opt/homebrew/bin/pinentry-mac` (or equivalent) to
   `~/.gnupg/gpg-agent.conf`, prompting before overwriting a different value.
3. Reloads gpg-agent.
4. Resolves `gpgconf --list-dirs agent-extra-socket` and persists the absolute
   path to `~/.cloister/state/gpg-forward-host-socket` for the registry to
   read.

The extra-socket is the *restricted* gpg-agent endpoint: it forbids key
generation and key export so a compromised VM cannot escalate beyond signing.

### Per-profile provisioning

When `GPGSigning=true`, provisioning runs `DeployGPGKeys`:

- Imports the host's public key into the VM's default keyring (`~/.gnupg`).
- Marks ownertrust as `ultimate` for that key.
- Writes `~/.gnupg/gpg.conf` with `no-autostart` so gpg never spawns an
  in-VM agent that would shadow the forwarded socket.
- Drops `/etc/ssh/sshd_config.d/cloister-gpg.conf` with
  `StreamLocalBindUnlink yes` and reloads sshd, so the forwarded socket can
  rebind cleanly across SSH sessions.

No private key material is shipped, no `GNUPGHOME` redirection, no
`~/.gnupg-local/` copy.

### Lifecycle integration

`gpg-forward` is registered in the tunnel `Builtins` list with
`HealthCheck: "socket"` and `RequiresFlag: "GPGSigning"`. Every `cloister
<profile>` entry runs `DiscoverForProfile` → `StartAll`, which (for socket
builtins) probes the host extra-socket via `os.Stat`, resolves the VM's
`$HOME` over SSH, and spawns `ssh -fN -R <guest-socket>:<host-socket>` with
`ExitOnForwardFailure=yes` so a failed bind surfaces immediately.

Because the registry runs on every entry, `cloister stop <profile>` followed
by `cloister <profile>` re-establishes forwarding without re-provisioning.

Setting `GPGSigning=true` is itself the consent signal for the `gpg-forward`
tunnel: profiles do not need a separate `tunnel_policy: [gpg-forward]` entry,
and a deny-all policy (e.g. the headless default) does not block the
flag-gated forward.

### Failure modes

- **Preflight not run:** `gpg-forward` reports as unavailable with the install
  hint `cloister setup gpg-forward`. Other tunnels are unaffected.
- **Host gpg-agent dead:** signing fails with a clean error inside the VM
  rather than hanging.
- **VM restart:** the forwarded socket binds afresh; the macOS Keychain
  retains the cached passphrase, so signing succeeds without a prompt.

### Security

- No private key material in the VM. `~/.gnupg/private-keys-v1.d/` stays
  empty (covered by an integration test).
- The forwarded socket is the restricted extra-socket, not the full agent
  socket — key generation and export are refused by the host agent
  (covered by an integration test).
- Passphrases are entered on the host via pinentry-mac and cached in the
  macOS Keychain; they never appear in VM state or backups.

## Terminal Integration

### iTerm2 (auto-detected via `$TERM_PROGRAM`)

- **Background color**: Per-profile hex color via `\033]Ph<hex>\033\\`
- **Tab title**: `\033]1;✱ Claude Code [<profile>]\007` — profile identity visible in Window menu
- **Window title**: `\033]2;cloister: <profile>\007`
- Auto-assigned colors for profiles without explicit color config (fixed palette of 8 visually distinct dark colors)

### Non-iTerm terminals

- Entry banner: `═══ cloister: work ═══`
- Shell prompt prefix via `PS1` modification in VM bashrc

## Backup & Restore

### What gets preserved

| Backed up | Why |
|-----------|-----|
| `~/.claude/projects/` | Conversation history, project memory |
| `~/.claude/tasks/` | Structured task data |
| `~/.claude/file-history/` | File version snapshots |
| `~/.claude/settings.json`, `history.jsonl`, `.claude.json` | Per-profile config |

| Excluded | Why |
|----------|-----|
| `plugins/`, `skills/`, `agents/` | Host-mounted, survive rebuilds |
| `cache/`, `telemetry/`, `debug/` | Ephemeral, regenerated |
| `~/.gnupg/` | Only the public key lives here; private keys never enter the VM |

### Storage

`~/.cloister/backups/<profile>/<timestamp>.tar.gz` — 5 retained per profile, oldest pruned automatically.

### Rebuild cycle

```
cloister rebuild work
→ backup work
→ delete work (destroy VM)
→ create work (re-provision with same config from ~/.cloister/config.yaml)
→ restore work (latest backup)
→ print: "Run 'claude login' to re-authenticate"
→ if GPG enabled: re-import host public key (automatic, no backup needed); the gpg-forward tunnel re-establishes on next entry
```

## Error Handling

- **All operations are idempotent**: retrying a failed command is always safe
- **Provisioning failures**: the VM is left running in its current state. The error is printed with the failing command. The user can SSH in to fix manually or run `cloister rebuild`
- **Tunnel drops during session**: tunnels are not monitored continuously. If a tunnel drops, the shim inside the VM falls back gracefully (op-forward shim falls back to local `op`, clipboard shim reports "no image found"). Re-entering the profile (`cloister <profile>`) re-establishes tunnels
- **Disk space exhaustion during backup**: backup is aborted, partial file is cleaned up, existing backups are preserved
- **Colima failures**: error output from Colima is captured and presented with a generic "VM failed to start" message plus a `--verbose` flag hint for debugging

## VM Architecture (internal, not user-facing)

Each profile maps to a Colima VM:
- Name: `colima-cloister-<profile>`
- VM type: `vz` (Apple Virtualization Framework)
- Mount type: `virtiofs`
- Arch: `aarch64` (arm64 on Apple Silicon, amd64 on Intel)
- Image: Ubuntu LTS (colima-core default)
- Minimum Colima version: 0.8.0

### Colima interaction

cloister uses `colima` as a subprocess. Structured data is obtained via `colima list --json`. VM lifecycle uses `colima start`, `colima stop`, `colima delete`, `colima ssh`. All Colima output is suppressed in normal mode and shown with `--verbose`.

### Mount strategy

| Host path | VM path | Mode | Purpose |
|-----------|---------|------|---------|
| `~/code` | `/Users/<user>/code` + `~/code` symlink | read-write | Development workspace |
| `~/.ssh` | `~/.ssh` symlink | read-only (remount) | SSH keys |
| `~/.gnupg` | `~/.gnupg` (virtiofs) | read-only (remount) | Public keyring source (private keys never leave the host; signing is forwarded via gpg-agent extra-socket) |
| `~/.claude/plugins` | `~/.claude/plugins` | read-write (interactive) / read-only (headless) | Shared plugins |
| `~/.claude/skills` | `~/.claude/skills` | read-write (interactive) / read-only (headless) | Shared skills |
| `~/.claude/agents` | `~/.claude/agents` | read-write (interactive) / read-only (headless) | Shared agents |
| `~/Downloads` | `~/Downloads` symlink | read-only (remount) | File access |
| `~/.ollama/models` | `~/.ollama/models` | read-only (remount) | Ollama model cache (stack-gated) |

Mounts are controlled by a per-profile `mount_policy` field (auto/none/explicit list). Interactive profiles default to all mounts; headless profiles default to code + claude extensions only. See `tunnel_policy` for equivalent tunnel consent.

### Isolated per VM

- `~/.claude/` (except plugins/skills/agents) — org credentials, CLAUDE.md, conversation history
- `~/.gnupg/` (writable, public key only) — populated by `DeployGPGKeys`; private keys are never copied here

## Self-Update

Same pattern as op-forward:
- `cloister self-update` queries GitHub Releases, downloads platform binary, replaces in-place
- Also available via `brew upgrade ekovshilovsky/tap/cloister`
- Release workflow: tag-driven, builds darwin × amd64/arm64 (macOS-only — Linux would require a different VM backend)

## Tech Stack

- **Language**: Go
- **CLI framework**: Cobra (structured commands, auto-generated help, shell completion)
- **Config**: `gopkg.in/yaml.v3` for config parsing
- **VM backend**: Colima CLI (invoked as subprocess, output parsed/suppressed)
- **Build**: Same release pipeline as op-forward (GitHub Actions, ldflags version injection, Homebrew tap)

## Repository

`github.com/ekovshilovsky/cloister` — public, MIT license.

Homebrew: `brew install ekovshilovsky/tap/cloister`

## V1 Scope

### Included in v1

- Core lifecycle: create, enter, stop, delete, status
- Memory management with budget enforcement
- Profile creation wizard with defaults + non-interactive mode
- Provisioning stacks: `web`, `cloud`, `python` (most common)
- Custom provisioning hook (`provision.sh`)
- Tunnel auto-discovery and guided setup for clipboard, op-forward, audio
- Backup/restore/rebuild
- iTerm2 integration (color + tab title)
- Self-update
- `--json` and `--list-options` for AI-friendliness

### Deferred to v2

- Additional stacks: `dotnet`, `go`, `rust`, `data` (users can install via `provision.sh` in v1)
- `add-stack` command (v1 users rebuild to add stacks)
- Custom tunnel configuration in config.yaml
- Shell completion generation
- Headless agent mode (see below)

## Headless Agent Mode (v2)

### Problem

Autonomous AI agents like [OpenClaw](https://openclaw.ai/) run 24/7, executing shell commands, controlling browsers, managing cron jobs, and making API calls. Their security track record is poor — [512 vulnerabilities found in audit](https://www.giskard.ai/knowledge/openclaw-security-vulnerabilities-include-data-leakage-and-prompt-injection-risks), WebSocket RCE giving host-level access, [malicious skills stealing credentials](https://adversa.ai/blog/openclaw-security-101-vulnerabilities-hardening-2026/). The community consensus is to never run these agents on bare metal.

Docker containers are the current minimum recommendation, but they share the host kernel and have a history of container escapes. cloister's VM isolation is fundamentally stronger — separate kernel, separate process space, explicit mount boundaries.

### Why cloister is a natural fit

cloister already provides everything an isolated agent runtime needs:

- **Filesystem containment**: agents can only access `~/code` (mounted workspace), not the host filesystem
- **Credential isolation**: SSH/GPG keys are read-only; agent can't exfiltrate or modify them
- **Network isolation**: services inside the VM (e.g., OpenClaw's gateway on port 3000) are unreachable from the host unless explicitly tunneled — this blocks the WebSocket RCE class of attacks entirely
- **Kill switch**: `cloister stop <agent-profile>` terminates all processes including rogue cron jobs
- **Memory budget**: prevents runaway agents from consuming all system resources
- **API key security**: instead of storing API keys in plaintext env vars, agents can use op-forward to retrieve credentials via 1Password with biometric approval

### CLI surface

```
cloister create ci-agent --headless --stack web
cloister agent start ci-agent --command "<agent command>"
cloister agent start ci-agent --openclaw           # shortcut: starts OpenClaw gateway
cloister agent status                              # show running headless agents
cloister agent logs ci-agent                       # tail agent stdout/stderr
cloister agent logs ci-agent --follow              # stream logs
cloister agent stop ci-agent                       # SIGTERM → SIGKILL after 10s
cloister agent restart ci-agent                    # stop + start
```

### Profile entry for headless profiles

`cloister <headless-profile>` still works — it SSHs into the VM for debugging/inspection. But the primary interaction model is `agent start/stop/logs`, not interactive shell sessions.

### The `--headless` flag

When a profile is created with `--headless`:
- No iTerm color/title changes on entry (it's not an interactive session)
- The VM starts without an SSH session attached
- `agent start` runs the command inside the VM via `colima ssh -p <profile> -- <command>` with output redirected to a log file
- The agent process is managed via PID file inside the VM
- If the VM stops (manual or memory eviction), the agent dies with it

### OpenClaw-specific support

`cloister agent start <profile> --openclaw` is a convenience shortcut that:
1. Checks if OpenClaw is installed in the VM (installs if missing via the official install script)
2. Starts the OpenClaw gateway inside the VM on port 3000
3. Does NOT forward port 3000 to the host (security: gateway stays VM-internal)
4. Prints: "OpenClaw running inside VM. Connect via: cloister <profile> then open http://localhost:3000"

The VMs are headless (no display server) so the web UI must be accessed via port forwarding to the host Mac's browser. The `--expose` flag creates an SSH local forward:
```
cloister agent start ci-agent --openclaw --expose 3000
```
This forwards VM port 3000 to `http://localhost:3000` on the host, with a warning:
```
⚠ Exposing port 3000 to host. OpenClaw's web UI will be accessible at http://localhost:3000
  Ensure AUTH_PASSWORD is set in your OpenClaw config.
  This port is only accessible from your Mac (loopback), not the network.
```
Without `--expose`, the gateway runs inside the VM but has no network path to the host — the most secure default. Users who only interact with OpenClaw via Telegram/Slack/Discord bots don't need the web UI at all.

### Security model for headless agents

| Threat | Mitigation |
|--------|-----------|
| Agent accesses host filesystem | VM boundary — only `~/code` is mounted, read-only mounts enforced for SSH/GPG |
| Agent exfiltrates API keys | Keys provided via op-forward (biometric per-request) rather than plaintext env vars |
| Agent starts rogue network services | Services are VM-internal; not reachable from host or network unless `--expose` |
| Agent spawns persistent cron jobs | `cloister stop` kills the VM, terminating all cron jobs |
| Agent consumes excessive resources | Memory budget applies; headless profiles count toward the budget |
| Malicious OpenClaw skill/plugin | Skill runs inside VM; cannot access host resources beyond mounted paths |
| WebSocket/RCE vulnerability in gateway | Gateway port not forwarded to host by default; attacker has no network path |
| Agent modifies SSH keys | `~/.ssh` is read-only (remount enforced) |

### Agent persistence

Headless agents should survive VM restarts (e.g., after a host reboot). This is handled by:
- Storing the agent command in `~/.cloister/config.yaml` under the profile
- On `colima start`, if the profile is headless and has a configured agent command, auto-start the agent
- `cloister agent stop` clears the auto-start flag; `cloister stop` does not (so the agent restarts on next VM boot)

```yaml
profiles:
  ci-agent:
    headless: true
    memory: 4
    stacks: [web]
    agent:
      command: "openclaw"
      auto_start: true
      expose: []  # no ports forwarded by default
```

### Log management

Agent logs are stored at `~/.cloister/logs/<profile>.log` on the host (streamed from the VM via SSH). Logs are rotated: 5 files, 10MB each. `cloister agent logs <profile>` reads from this file.

### Non-interactive / AI mode

```
$ cloister agent status --json
{
  "agents": [
    {
      "profile": "ci-agent",
      "state": "running",
      "pid": 12345,
      "uptime": "3h 42m",
      "memory": "4GB",
      "command": "openclaw",
      "log_file": "~/.cloister/logs/ci-agent.log"
    }
  ]
}
```

## Success Criteria

### v1
1. A new user can go from `brew install` to an isolated Claude Code session in under 5 minutes
2. Zero mention of Colima in any user-facing surface
3. Full non-interactive mode for AI-driven setup
4. Memory budget prevents runaway VM resource consumption
5. Session data survives VM rebuilds via backup/restore
6. Tunneled services (clipboard, 1Password, audio) work automatically when host services are present

### v2 (headless agent mode)
7. A user can run OpenClaw or any autonomous agent inside a cloister VM with a single command
8. Agent gateway/services are VM-internal by default — no host network exposure without explicit opt-in
9. API keys are never stored in plaintext inside the VM when op-forward is available
10. `cloister stop` is a reliable kill switch that terminates all agent processes and cron jobs
