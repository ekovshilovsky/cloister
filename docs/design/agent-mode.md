# cloister agent: Headless Agent Mode

**Date:** 2026-03-22
**Status:** Approved
**Scope:** New `--headless` and `--openclaw` create flags, `cloister agent` command tree, Docker-in-VM process management, port forwarding

## Problem

Autonomous AI agents like OpenClaw execute shell commands, manage files, control browsers, and act on the user's behalf unattended. Running them on bare metal or even in a basic VM with full user permissions exposes SSH keys, API credentials, and host filesystems to compromise. There is no `cloister agent` command despite it being reserved and spec'd in the v2 design — users cannot create, start, or manage headless agent profiles.

## Solution

A headless agent mode that runs OpenClaw inside a Docker container inside a cloister VM, with restricted mounts, no tunnels by default, no interactive shell access, and credentials provided via 1Password (op-forward) rather than plaintext env vars. Managed entirely through `cloister agent <profile> <command>`.

## Architecture

### Three-Layer Isolation

```
macOS host
    ↓
cloister VM (headless — restricted mounts, no tunnels by default)
    ↓
Docker container (dropped capabilities, scoped volumes, non-root)
    ↓
OpenClaw process
```

Each layer constrains the one below it:

- **VM layer** — separate kernel, explicit mount boundaries, no shell access for headless profiles
- **Docker layer** — dropped Linux capabilities, read-only rootfs areas, non-root user, published ports bound to localhost only
- **Process layer** — OpenClaw runs inside the container with no direct host access

### Security Model

| Threat | Mitigation |
|--------|-----------|
| Agent accesses host filesystem | VM boundary — only `~/code` mounted, Claude extensions read-only |
| Agent exfiltrates API keys | Credentials via op-forward (biometric per-request), not plaintext env vars |
| Agent starts rogue network services | Docker ports bound to VM localhost; VM ports unreachable from host unless SSH tunnel |
| Agent spawns persistent cron jobs | `cloister stop <profile>` kills the VM, terminating all processes |
| Agent consumes excessive resources | VM memory budget applies; Docker `--memory` limit |
| Malicious OpenClaw skill/plugin | Runs inside Docker container inside VM; double containment |
| WebSocket/RCE vulnerability in gateway | Gateway port not forwarded to host by default; SSH tunnel is ephemeral and explicit |
| Agent modifies SSH keys | `~/.ssh` not mounted in headless profiles |
| Agent reads other agent's data | Per-profile storage at `~/.cloister/agents/<profile>/`; no shared state |

## CLI Surface

### Profile Creation

```
cloister create <name> --headless --openclaw
```

`--headless` sets the profile to headless mode (no interactive shell access). `--openclaw` implies `--headless` and configures OpenClaw-specific defaults (web stack, Docker image, agent block in config).

### Agent Management

```
cloister agent <profile> start
cloister agent <profile> stop
cloister agent <profile> restart
```

### Observability

```
cloister agent status                        # all running agents
cloister agent <profile> status              # single agent detail
cloister agent <profile> logs
cloister agent <profile> logs --follow
```

### Port Forwarding

```
cloister agent <profile> forward <port>      # SSH tunnel: Mac:port → VM:port
cloister agent <profile> close <port>        # tear down the tunnel
cloister agent <profile> close               # close all forwards for this profile
```

### Blocked Commands

`cloister <headless-profile>` (no subcommand) returns an error:
```
"<profile>" is a headless agent profile. Use 'cloister agent <profile>' to manage it.
```

## Config Schema

### Profile entry in `~/.cloister/config.yaml`

```yaml
profiles:
  openclaw:
    headless: true
    memory: 4
    disk: 40
    stacks: [web]
    tunnel_policy: [op-forward]
    mount_policy: [code, claude-plugins, claude-skills, claude-agents]
    agent:
      type: openclaw
      image: openclaw/openclaw:latest
      ports: [3000]
      auto_start: true
      env: {}
```

### Agent block fields

| Field | Type | Description |
|-------|------|-------------|
| `type` | string | Agent runtime identifier. `openclaw` for now, extensible. |
| `image` | string | Docker image to run. Defaults based on type, overridable. |
| `ports` | []int | Ports published to VM localhost (`-p 127.0.0.1:<port>:<port>`). |
| `auto_start` | bool | When true, agent starts automatically on VM boot. |
| `env` | map | Optional env var overrides injected into the container. |

### Host-side storage

Per-profile isolated storage at `~/.cloister/agents/<profile>/`:

```
~/.cloister/agents/<profile>/
  └── openclaw/              # mounted as /home/node/.openclaw in container
      ├── openclaw.json      # OpenClaw config
      ├── .env               # secrets (or SecretRefs pointing to 1Password)
      ├── memory/            # OpenClaw's persistent memory files
      └── tmp/               # Chromium browser cache, scratch files
```

The workspace mount comes from the profile's `start_dir` (e.g., `~/code/openclaw`), mounted as `/home/node/.openclaw/workspace` in the container.

Each profile gets its own directory — no data mixing between agents.

### Agent data mount (host → VM → Docker)

The agent data directory must traverse three layers: macOS host → VM filesystem → Docker container.

**Colima mount (host → VM):** When starting the VM for a headless profile with an agent block, `vm.BuildMounts` adds the agent data directory as an additional writable mount. This is not part of the standard mount catalog or mount policy — it is unconditionally added (like the workspace mount) when the profile has an `agent` config block:

```
Host: ~/.cloister/agents/<profile>/openclaw/
  → VM: /home/<user>/.openclaw/
```

**Docker volume (VM → container):** The Docker run command maps the VM path into the container:

```
VM: /home/<user>/.openclaw/ → Docker: /home/node/.openclaw
VM: <workspace>             → Docker: /home/node/.openclaw/workspace
VM: /home/<user>/.openclaw/tmp → Docker: /tmp
VM: /home/<user>/.openclaw/tmp/browser-cache → Docker: /home/node/.cache
```

This means `vm.BuildMounts` needs modification to accept the agent data directory path and append it to the mount list for headless agent profiles.

### State tracking

Ephemeral state at `~/.cloister/state/`:

| File | Purpose |
|------|---------|
| `<profile>.agent.container` | Docker container ID |
| `<profile>.forward.<port>.pid` | SSH tunnel PID for a specific port forward (one file per forward, matching the existing per-tunnel PID file pattern) |

## Docker Configuration

### Container run command

```bash
docker run -d --name <profile> \
  --cap-drop ALL --cap-add SYS_ADMIN \
  --user 1000:1000 \
  --shm-size=2g \
  -v /home/user/.openclaw/tmp:/tmp \
  -v /home/user/.openclaw/tmp/browser-cache:/home/node/.cache \
  -p 127.0.0.1:3000:3000 \
  -v <host-agent-dir>/openclaw:/home/node/.openclaw \
  -v <workspace>:/home/node/.openclaw/workspace \
  --log-opt max-size=10m --log-opt max-file=5 \
  openclaw/openclaw:latest
```

### Capability rationale

- `--cap-drop ALL` — drops all Linux capabilities for minimal privilege
- `--cap-add SYS_ADMIN` — required for Chromium's sandboxing (namespaces). The alternative (`--no-sandbox`) is less secure. The VM boundary is the primary containment.
- `--user 1000:1000` — non-root execution inside the container
- `--shm-size=2g` — prevents Chromium shared memory exhaustion during rendering

### Temp file management

Chromium's browser cache and scratch files are stored at `~/.cloister/agents/<profile>/openclaw/tmp/` on the host, mounted into the container. This avoids shared `/tmp` and keeps temp files scoped per-profile.

A cron job inside the VM (installed during provisioning) cleans files older than 7 days:

```bash
find /home/user/.openclaw/tmp -type f -mtime +7 -delete
```

### Port publishing

Ports listed in `agent.ports` are published to VM localhost only (`-p 127.0.0.1:<port>:<port>`). They are NOT reachable from the host or network. Access requires an explicit SSH tunnel via `cloister agent <profile> forward <port>`.

## Credential Management

### Preferred: op-forward + OpenClaw SecretRef

When op-forward is available on the host and the profile's `tunnel_policy` includes `op-forward`:

1. The op-forward tunnel is established when the agent starts
2. The op-forward shim is deployed inside the VM
3. OpenClaw's `openclaw.json` uses SecretRef objects pointing to 1Password:
   ```json
   {
     "ai": {
       "apiKey": { "source": "env", "provider": "1password", "id": "ANTHROPIC_API_KEY" }
     }
   }
   ```
4. When OpenClaw needs a credential, `op` calls through the tunnel to the host, requiring biometric approval
5. Keys never exist as plaintext on disk inside the VM

### Fallback: env var injection

When op-forward is not available, API keys can be passed via the `agent.env` config block:

```yaml
agent:
  env:
    ANTHROPIC_API_KEY: "sk-ant-..."
```

These are injected as Docker environment variables at container start. Less secure (readable inside the container) but functional for users without 1Password.

`cloister create --openclaw` auto-detects op-forward presence on the host and sets `tunnel_policy: [op-forward]` when available.

## Process Lifecycle

### `cloister agent <profile> start`

1. Load profile config, verify it's headless with an agent block
2. If VM not running: check memory budget, start VM with headless mounts/tunnels
3. Verify Docker daemon is operational inside the VM (`docker info`). If not, print diagnostic guidance and exit.
4. Set up tunnels per `tunnel_policy` (e.g., op-forward)
5. Deploy op-forward shims if tunnel is active
6. Run Docker container (detached) inside VM via `docker run -d`
7. Store container ID in state file
8. Set `auto_start: true` in config (intentional: `start` always enables auto-start; use `cloister config` to set `auto_start: false` if you want manual-only starts)
9. Print status and next-steps guidance

### `cloister agent <profile> stop`

1. Run `docker stop <container>` inside VM (SIGTERM → 10s → SIGKILL)
2. Run `docker rm <container>` to clean up
3. Set `auto_start: false` in config
4. Close all active SSH forwards for this profile
5. Does NOT stop the VM

### `cloister stop <profile>`

1. Close all active agent SSH forwards for this profile (via `agent.CloseAllForwards`)
2. Stop the entire VM — the agent container dies with it
3. `auto_start` is NOT changed — the agent restarts on next VM boot
4. Existing reverse tunnels (op-forward, clipboard, etc.) are closed by the existing `tunnel.StopAll()` call — these are separate from agent local forwards and managed independently

### `cloister agent <profile> restart`

Equivalent to stop + start.

### Auto-start on VM boot

When the VM starts (via `cloister agent <profile> start` or host reboot recovery), cloister checks if `auto_start: true` and runs the Docker container automatically. Implemented in the enter flow: when entering a headless profile's VM, if `auto_start` is set, start the container instead of opening a shell.

## Port Forwarding

### `cloister agent <profile> forward <port>`

1. Verify the port is in the agent's `ports` list
2. Create SSH tunnel: `ssh -L <port>:localhost:<port> -N -f` using the VM's SSH config
3. Store the SSH PID in `~/.cloister/state/<profile>.forward.<port>.pid`
4. Print access URL and security warning

### `cloister agent <profile> close <port>`

1. Read PID from `~/.cloister/state/<profile>.forward.<port>.pid`
2. Kill the SSH tunnel process
3. Remove the PID file

### `cloister agent <profile> close` (no port)

Closes all active forwards for the profile by scanning `~/.cloister/state/<profile>.forward.*.pid` files.

### Lifecycle

Forwards are ephemeral — they do not survive VM restart or host reboot. They are stored in state (not config) and must be explicitly re-created.

## Observability

### `cloister agent status` (no profile)

```
PROFILE     STATE      UPTIME    IMAGE
openclaw    running    3h 42m    openclaw/openclaw:latest
```

Lists all headless profiles with running agents. `--json` flag for machine-readable output.

### `cloister agent <profile> status`

```
Profile:    openclaw
State:      running
Uptime:     3h 42m
Image:      openclaw/openclaw:latest
Ports:      3000 (VM-internal)
Forwards:   3000 → localhost:3000 (active)
Auto-start: true
```

### `cloister agent <profile> logs`

Runs `docker logs <container>` inside the VM. Shows last 100 lines by default.

### `cloister agent <profile> logs --follow`

Runs `docker logs -f <container>` inside the VM. Streams until Ctrl+C.

Log rotation is handled by Docker's log driver (`--log-opt max-size=10m --log-opt max-file=5`).

## Create Flow

### `cloister create <name> --headless --openclaw`

1. Validate profile name
2. `--openclaw` implies `--headless` — error if `--headless` is explicitly false
3. Auto-detect op-forward on host → set `tunnel_policy: [op-forward]` or leave as default
4. Auto-select stacks: `[web]` (Node.js requirement for OpenClaw)
5. Create host-side agent data directory: `~/.cloister/agents/<name>/openclaw/`
6. Create profile in config with agent block
7. Start VM with headless mount/tunnel policies
8. Provision: base tools + web stack
9. Pull OpenClaw Docker image inside VM: `docker pull openclaw/openclaw:latest`
10. Deploy VM config (cloister-vm toolkit)
11. Install tmp cleanup cron job
12. Print next-steps guidance:
    ```
    Profile "<name>" created.

    Next steps:
      1. Start the agent:    cloister agent <name> start
      2. Forward the web UI:  cloister agent <name> forward 3000
      3. Open in browser:     http://localhost:3000
      4. Complete the onboarding wizard to connect messaging platforms
      5. Close the forward:   cloister agent <name> close 3000
    ```

### `--headless` without `--openclaw`

Creates a headless profile without OpenClaw-specific configuration. No agent block, no Docker image pull. The user can manually configure `agent.type` and `agent.image` in the config for other agent runtimes.

## Files

### New files

| File | Purpose |
|------|---------|
| `cmd/agent.go` | `cloister agent` command tree: start, stop, restart, status, logs, forward, close |
| `internal/agent/manager.go` | Docker container lifecycle: start, stop, status, inspect |
| `internal/agent/forward.go` | SSH tunnel management: create, destroy, list |
| `internal/agent/state.go` | State file I/O: container IDs, tunnel PIDs |
| `internal/agent/openclaw.go` | OpenClaw-specific defaults: image, ports, Docker flags, install logic |
| `internal/provision/scripts/agent-setup.sh` | Provisioning script: pull Docker image, install tmp cleanup cron. Invoked from `provision.Run()` as a new step after stack provisioning, gated on `p.Agent != nil`. |

### Modified files

| File | Change |
|------|--------|
| `cmd/create.go` | Add `--headless` and `--openclaw` flags, agent block creation. `--openclaw` implies `--headless` and `--defaults` (skips interactive wizard). |
| `cmd/enter.go` | Block entry for headless profiles with error message |
| `cmd/stop.go` | Add agent forward cleanup: call `agent.CloseAllForwards(profile)` before stopping VM |
| `cmd/root.go` | Register `agent` subcommand |
| `internal/config/config.go` | Add `AgentConfig` struct to `Profile` |
| `internal/provision/engine.go` | Add agent setup step in `Run()`: if `p.Agent != nil`, run `agent-setup.sh` with image name as env var |
| `internal/vm/mount.go` | Extend `BuildMounts` to accept optional agent data dir path, appended unconditionally for agent profiles |
| `internal/profile/profile.go` | Keep `"agent"` in reserved names (prevents profile name collision with the command) |

## Dependencies

- Docker (already provisioned in every cloister VM via Colima)
- OpenClaw Docker image (`openclaw/openclaw:latest`)
- op-forward (optional, for secure credential injection)
- SSH (already available via Colima's SSH config)
- cloister-vm toolkit (already installed via v0.4.0)
