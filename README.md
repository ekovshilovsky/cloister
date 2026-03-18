# cloister

[![CI](https://github.com/ekovshilovsky/cloister/actions/workflows/ci.yml/badge.svg)](https://github.com/ekovshilovsky/cloister/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/ekovshilovsky/cloister)](https://github.com/ekovshilovsky/cloister/releases/latest)
[![Go Report Card](https://goreportcard.com/badge/github.com/ekovshilovsky/cloister)](https://goreportcard.com/report/github.com/ekovshilovsky/cloister)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

Isolated VM environments for AI coding agents and multi-account separation.

![cloister demo](demo.gif)

## Why cloister?

**Multi-account isolation.** Claude Code stores credentials, conversation history, and project config in `~/.claude`. If you work across multiple organizations or clients, every session shares the same identity. The `CLAUDE_CONFIG_DIR` workaround is [officially broken](https://github.com/anthropics/claude-code/issues/16103). cloister gives each account its own isolated environment with separate credentials, CLAUDE.md, and conversation history.

**Secure sandboxing.** Claude Code has full shell access to your machine. cloister runs it inside a VM with explicit filesystem boundaries — only your code workspace is mounted (read-write), while SSH keys and GPG keys are read-only. If something goes wrong, `cloister stop` is an instant kill switch.

**Autonomous agent containment.** Tools like [OpenClaw](https://openclaw.ai/) run AI agents 24/7 with shell access, browser control, and cron scheduling. Their [security track record](https://www.giskard.ai/knowledge/openclaw-security-vulnerabilities-include-data-leakage-and-prompt-injection-risks) makes bare-metal deployment dangerous. cloister's VM isolation is stronger than Docker (separate kernel, not just namespace isolation) — services inside the VM are unreachable from the host unless explicitly tunneled.

## How It Works

cloister creates lightweight macOS VMs (via Apple Virtualization Framework) where each profile gets its own isolated `~/.claude` while sharing your code workspace, SSH keys, and Claude Code plugins.

```
macOS host
├── ~/Code/                    ← shared across all profiles (read-write)
├── ~/.ssh/                    ← shared (read-only in VMs)
├── ~/.claude/plugins/         ← shared (install once, available everywhere)
│
├── cloister: work             ← isolated ~/.claude, own credentials
├── cloister: personal         ← isolated ~/.claude, own credentials
└── cloister: client-x         ← isolated ~/.claude, own credentials
```

## Quick Start

```bash
# Install
brew install ekovshilovsky/tap/cloister

# Create a profile
cloister create work

# Enter it
cloister work
# You're now in an isolated environment. Run: claude login
```

That's it. Your code is at `~/Code`, your SSH keys work, and Claude Code is installed. Each profile has its own credentials and conversation history.

## Commands

```
cloister create <profile>          Create a new isolated profile
cloister <profile>                 Enter a profile (starts VM if needed)
cloister stop [profile|all]        Stop environment(s) to free memory
cloister status                    Show all profiles, memory usage, tunnel health
cloister delete <profile>          Destroy a profile and its data
cloister update [profile|all]      Update Claude Code and system packages
cloister backup [profile|all]      Back up session data (history, settings)
cloister restore <profile>         Restore from backup
cloister rebuild <profile>         Backup, destroy, re-provision, restore
cloister setup <service>           Guided install for optional services
cloister add-stack <profile> <s>   Add toolchain to an existing profile
cloister config                    Edit configuration
cloister self-update               Update cloister itself
cloister version                   Print version
```

## Profile Creation

### Interactive (default)

```
$ cloister create work

Creating profile "work"...
Use defaults? (4GB RAM, ~/Code, auto color) [Y/n]: n

Memory allocation (GB) [4]: 6
Starting directory [~/Code]: ~/Code/my-project
Background color (hex, no #) [auto]: 0a1628
Provisioning stacks (web,cloud,dotnet,python,go,rust,data) [none]: web,cloud
Enable GPG commit signing? [y/N]: y

Profile "work" created. Enter with: cloister work
```

### Non-interactive

```bash
cloister create work --defaults
cloister create work --memory 6 --start-dir ~/Code/my-project --stack web,cloud --gpg-signing
```

### AI-friendly

```bash
cloister create --list-options --json   # discover all configurable options
cloister status --json                  # machine-readable status
```

## Provisioning Stacks

Stacks install development toolchains into your profile:

| Stack | What it installs |
|-------|-----------------|
| `web` | Playwright + Chromium, GitHub CLI, Vercel CLI |
| `cloud` | AWS CLI, gcloud, Azure CLI, Terraform |
| `python` | Python via pyenv, pip, venv |
| `dotnet` | .NET SDK, mssql-tools, PostgreSQL client |
| `go` | Go (official tarball) |
| `rust` | Rust via rustup, cargo |
| `data` | mongosh, PostgreSQL client |

Stacks are composable: `--stack web,cloud,python`

Version overrides: `--dotnet-version 8`, `--python-version 3.12`, `--go-version 1.24`

The base install (always included) provides: git, Node.js LTS, pnpm, Claude Code, and GPG tools.

## Memory Management

cloister tracks memory usage and prevents runaway VM consumption:

```
$ cloister status

PROFILE      STATE     MEMORY   IDLE     STACKS
personal     running   4GB      3h       web
work         running   6GB      active   web,cloud

Budget: 10GB / 22GB used
Tunnels: clipboard ✓  op-forward ✓  audio ✗
```

When starting a profile would exceed the budget, cloister suggests stopping idle profiles:

```
⚠ Memory budget exceeded: 26GB would be used of 22GB budget
  Stop "personal" to free 4GB? [Y/n]:
```

## Optional Services

cloister auto-detects host services and tunnels them into VMs:

| Service | What it enables | Install |
|---------|----------------|---------|
| [cc-clip](https://github.com/ShunmeiCho/cc-clip) | Clipboard image pasting (Ctrl+V screenshots) | `brew install ShunmeiCho/tap/cc-clip` |
| [op-forward](https://github.com/ekovshilovsky/op-forward) | 1Password CLI with Touch ID | `brew install ekovshilovsky/tap/op-forward` |
| PulseAudio | Voice dictation (`/voice`) | `brew install pulseaudio` |

Guided setup: `cloister setup op-forward`

## Backup & Restore

Session data (conversation history, project memory, settings) survives VM rebuilds:

```bash
cloister backup work                # back up session data
cloister rebuild work               # backup → destroy → re-provision → restore
```

5 backups retained per profile, oldest pruned automatically.

## Configuration

`~/.cloister/config.yaml`:

```yaml
memory_budget: 16

profiles:
  work:
    memory: 6
    start_dir: ~/Code/my-project
    color: "0a1628"
    stacks: [web, cloud]
    gpg_signing: true

tunnels:
  - name: my-service
    host_port: 9000
```

## Requirements

- macOS 13+ (Ventura)
- Apple Silicon or Intel Mac
- 16GB RAM recommended (12GB minimum)
- Homebrew

## How is this different from Docker?

Docker containers share the host kernel. A container escape gives access to your entire machine. cloister uses Apple's Virtualization Framework to create actual VMs — separate kernel, separate process space, explicit mount boundaries. This matters especially for running autonomous AI agents (see [Headless Agent Mode](docs/design/spec.md#headless-agent-mode-v2) in the spec).

## License

MIT
