# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability in cloister, please report it responsibly.

**Do not open a public GitHub issue for security vulnerabilities.**

Instead, use [GitHub's private vulnerability reporting](https://github.com/ekovshilovsky/cloister/security/advisories/new).

Include:
- Description of the vulnerability
- Steps to reproduce
- Potential impact
- Suggested fix (if any)

You will receive a response within 48 hours. Critical vulnerabilities will be patched and released within 7 days of confirmation.

## Security Model

cloister provides VM-level isolation using Apple's Virtualization Framework. Understanding the security boundaries is important:

### What cloister isolates

- **Credentials**: Each profile has its own `~/.claude` with separate auth tokens, conversation history, and CLAUDE.md
- **Processes**: Each profile runs in a separate VM with its own kernel and process space
- **Network services**: Services inside a VM are unreachable from the host unless explicitly tunneled

### What is intentionally shared

- **Code workspace** (`~/code`): Read-write across all profiles — this is by design for developer productivity
- **SSH keys** (`~/.ssh`): Read-only in VMs, enforced via post-boot remount
- **GPG keys** (`~/.gnupg`): Read-only in VMs, copied to a local writable keyring when GPG signing is enabled
- **Claude Code plugins** (`~/.claude/plugins`, `skills`, `agents`): Shared across profiles — read-write for interactive profiles, read-only for headless profiles to prevent lateral movement

### Threat model

cloister assumes:
- The macOS host is trusted
- SSH tunnels between host and VMs are secure (loopback only)
- The primary isolation boundary is the VM, not access control within the VM

cloister does **not** protect against:
- A compromised host machine
- Malicious code with read-write access to `~/code` affecting other profiles' workspaces
- Side-channel attacks between VMs on the same host

### Resource consent policies

Profiles control which host resources are accessible via `tunnel_policy` and `mount_policy` fields:

- **Interactive profiles** default to `auto` — all host services and directories are available
- **Headless profiles** default to `none` (tunnels) and a minimal set (mounts) — only the workspace and Claude extensions are mounted, with no SSH keys, GPG keys, or Downloads exposed
- Explicit whitelists (e.g., `tunnel_policy: [ollama]`) restrict access to named resources only

### Tunnel security

- All tunneled services (clipboard, 1Password, audio) bind to `127.0.0.1` only
- op-forward uses per-request biometric authentication (Touch ID)
- Tunnel ports inside VMs are established via SSH reverse forwarding with `ControlMaster=no` (dedicated connections)

### Headless agent mode (v2)

When running autonomous agents (e.g., OpenClaw) inside cloister VMs:
- Agent gateway ports are **not** forwarded to the host by default
- The `--expose` flag is required to make any VM service reachable from the host, with a security warning
- `cloister stop` terminates all processes including rogue cron jobs
- API keys can be provided via op-forward (biometric per-request) rather than plaintext environment variables

## Supported Versions

Security patches are applied to the latest release only. Upgrade to the latest version to receive fixes:

```bash
brew upgrade ekovshilovsky/tap/cloister
```
