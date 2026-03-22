# Cloister: Run Multiple Claude Code Accounts & AI Agents on One Mac

[![CI](https://github.com/ekovshilovsky/cloister/actions/workflows/ci.yml/badge.svg)](https://github.com/ekovshilovsky/cloister/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/ekovshilovsky/cloister)](https://github.com/ekovshilovsky/cloister/releases/latest)
[![Go Report Card](https://goreportcard.com/badge/github.com/ekovshilovsky/cloister)](https://goreportcard.com/report/github.com/ekovshilovsky/cloister)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

Isolated macOS VM environments for multiple Claude Code organizations and secure AI agent sandboxing.

![cloister demo](demo.gif)

## Why cloister?

**Multi-account isolation.** Claude Code stores credentials, conversation history, and project config in `~/.claude`. If you work across multiple organizations or clients, every session shares the same identity. The `CLAUDE_CONFIG_DIR` workaround is [officially broken](https://github.com/anthropics/claude-code/issues/16103). cloister gives each account its own isolated environment with separate credentials, CLAUDE.md, and conversation history.

**Secure sandboxing.** Claude Code has full shell access to your machine. cloister runs it inside a VM with explicit filesystem boundaries — only your code workspace is mounted (read-write), while SSH keys and GPG keys are read-only. If something goes wrong, `cloister stop` is an instant kill switch.

**Autonomous agent containment.** Tools like [OpenClaw](https://openclaw.ai/) run AI agents 24/7 with shell access, browser control, and cron scheduling. Their [security track record](https://www.giskard.ai/knowledge/openclaw-security-vulnerabilities-include-data-leakage-and-prompt-injection-risks) makes bare-metal deployment dangerous. cloister's VM isolation is stronger than Docker (separate kernel, not just namespace isolation) — services inside the VM are unreachable from the host unless explicitly tunneled.

## How It Works

cloister creates lightweight macOS VMs (via Apple Virtualization Framework) where each profile gets its own isolated `~/.claude` while sharing your code workspace, SSH keys, and Claude Code plugins.

```
macOS host
├── ~/code/                    ← shared across all profiles (read-write)
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

That's it. Your code is at `~/code`, your SSH keys work, and Claude Code is installed. Each profile has its own credentials and conversation history.

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
cloister update-config <profile>   Toggle settings (e.g. --claude-local)
cloister exec <profile> <cmd>      Run a command inside a VM
cloister config                    Edit configuration
cloister self-update               Update cloister itself
cloister version                   Print version
```

## Profile Creation

### Interactive (default)

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

### Non-interactive

```bash
cloister create work --defaults
cloister create work --memory 6 --start-dir ~/code/my-project --stack web,cloud --gpg-signing
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
| `ollama` | Ollama CLI (GPU inference via host tunnel) |

Stacks are composable: `--stack web,cloud,python,ollama`

Version overrides: `--dotnet-version 8`, `--python-version 3.12`, `--go-version 1.24`

The base install (always included) provides: git, Node.js LTS, pnpm, Claude Code, and GPG tools.

## Ollama Integration

The `ollama` stack enables local LLM inference inside cloister VMs using the host machine's GPU.

### Why host-tunneled inference?

Cloister VMs run Linux via Apple's Virtualization Framework. Apple does not expose Metal GPU access to guest operating systems through any hypervisor API — neither the Virtualization framework nor Hypervisor.framework supports GPU passthrough for Linux VMs. The only alternative is Vulkan translation via krunkit (Colima v0.10+, M3+ only), which routes through multiple translation layers (Vulkan in guest, Venus virtio-gpu, MoltenVK on host) and loses the performance advantages of native Metal compute.

Instead of running inference inside the VM, cloister tunnels the host's Ollama server into the VM via SSH reverse port forwarding. The host runs Ollama with native Metal acceleration; the VM's `ollama` CLI connects to `127.0.0.1:11434` which is transparently forwarded to the host. Models are loaded once on the host and shared across all VMs that have the `ollama` stack — no re-downloading, no per-VM GPU overhead.

### How it works

```
VM (Linux)                          macOS host
┌─────────────────────┐             ┌──────────────────────────┐
│ ollama run gemma3    │──tunnel──▶ │ Ollama server (Metal GPU)│
│ 127.0.0.1:11434     │   SSH -R   │ 127.0.0.1:11434          │
│                      │            │                          │
│ ~/.ollama/models ────│──mount───▶ │ ~/.ollama/models (blobs) │
│ (read-only)          │  virtiofs  │                          │
└─────────────────────┘             └──────────────────────────┘
```

- **Inference**: runs on the host's GPU via SSH tunnel (zero translation overhead)
- **Model cache**: host's `~/.ollama/models` mounted read-only into the VM so model metadata is accessible without duplication
- **Ollama server inside VM**: installed but disabled — the systemd service is stopped and disabled during provisioning so it doesn't compete with the host

### Recommended models

| Model | Size | RAM | Best for |
|-------|------|-----|----------|
| `qwen2.5-coder:7b` | 4.7 GB | 8 GB+ | Code review, generation, refactoring — purpose-built for development tasks |
| `qwen2.5-coder:3b` | 2 GB | 4 GB+ | Fast code completions on resource-constrained machines |
| `gemma3:4b` | 3 GB | 6 GB+ | General-purpose tasks with solid code understanding |
| `gemma3:27b` | 17 GB | 32 GB+ | Highest quality reasoning and code generation (requires high-memory Mac) |

For most users, **`qwen2.5-coder:7b`** is the best starting point — it runs fast on any Apple Silicon Mac with 8 GB RAM and handles the code-focused tasks (review, refactoring, test generation) that MCP tools like [pal-mcp-server](https://github.com/BeehiveInnovations/pal-mcp-server) delegate to local models.

### Setup

```bash
# Install Ollama on your Mac (if not already installed)
brew install ollama

# Pull a model (qwen2.5-coder:7b recommended for most setups)
ollama pull qwen2.5-coder:7b

# Create a profile with the ollama stack
cloister create dev --stack ollama

# Enter the profile — tunnel is established automatically
cloister dev

# Inside the VM, ollama commands use the host's server
ollama list                              # shows models from host
ollama run qwen2.5-coder:7b "hello"     # runs on host GPU
```

If Ollama is not running on the host when the profile is created, cloister prints a warning and proceeds — the CLI is installed in the VM and will connect once the host server is available and the tunnel is active.

### Local Claude Code (offline mode)

Claude Code can run entirely against your local Ollama instead of Anthropic's cloud API. This enables fully offline development with no API keys and no internet dependency.

```bash
# Create a profile with local Claude Code
cloister create dev --stack ollama --claude-local

# Or enable it on an existing profile
cloister update-config dev --claude-local

# Inside the VM, Claude Code uses the local model
claude --model qwen2.5-coder:7b

# Switch back to Anthropic cloud
cloister update-config dev --claude-cloud
```

Local mode uses Ollama's [Anthropic Messages API compatibility](https://docs.ollama.com/api/anthropic-compatibility) — Claude Code sends requests to the host's Ollama server through the tunnel, and Ollama translates them for the local model. Features like multi-turn conversations, tool calling, and vision are supported.

For advanced model routing (different models for different task types), see [claude-code-router](https://github.com/musistudio/claude-code-router) — an optional proxy that maps Claude's Sonnet/Haiku/Opus tiers to different local or cloud models.

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
| [PulseAudio](https://github.com/pulseaudio/pulseaudio) | Voice dictation (`/voice`) | `brew install pulseaudio` |

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
    start_dir: ~/code/my-project
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

Docker containers share the host kernel. A container escape gives access to your entire machine. Cloister uses Apple's Virtualization Framework to create actual VMs — separate kernel, separate process space, explicit mount boundaries.

This matters especially for autonomous AI agents like [OpenClaw](https://github.com/openclaw/openclaw) that run 24/7 with shell access, browser control, and cron scheduling. OpenClaw's [security track record](https://www.giskard.ai/knowledge/openclaw-security-vulnerabilities-include-data-leakage-and-prompt-injection-risks) (512 vulnerabilities in audit, WebSocket RCE, malicious skills) makes bare-metal or Docker deployment risky. Cloister's VM isolation contains the blast radius — services inside the VM are unreachable from the host unless explicitly tunneled, and `cloister stop` is an instant kill switch that terminates all processes including rogue cron jobs. See [Headless Agent Mode](docs/design/spec.md#headless-agent-mode-v2) in the spec.

## License

MIT
