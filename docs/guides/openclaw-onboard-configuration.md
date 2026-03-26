# OpenClaw Onboard Configuration for Cloister

Research conducted 2026-03-25 for cloister's Lume VM OpenClaw provisioning.

## Onboard Flow

`openclaw onboard` configures in order:

1. **AI Provider + Auth** — API key or OAuth for model access (required)
2. **Workspace** — agent file directory (default `~/.openclaw/workspace`)
3. **Gateway** — port (default 18789), bind address, auth mode
4. **Channels** — messaging platform integrations
5. **Daemon** — LaunchAgent on macOS for auto-start
6. **Health check** — verifies gateway starts
7. **Skills** — installs recommended skills

## Security Architecture (Onion Layers)

The VM is one layer. OpenClaw provides additional layers that should still be configured:

| Layer | What | Cloister Config |
|-------|------|-----------------|
| VM isolation | Separate kernel, mount boundaries | Lume macOS VM |
| Gateway bind | loopback-only by default | `--gateway-bind loopback` |
| Gateway auth | Token-based, fail-closed | Generate token, store via 1Password |
| Device pairing | Ed25519 challenge-response for native apps | Enabled by default (no `dangerouslyDisableDeviceAuth`) |
| Allowlists | Restrict which users can message the bot | Per-channel config |
| Exec approvals | Bind exact command/cwd/env for shell execution | Security audit flags permissive ones |
| Skills sandboxing | Limit tool access per skill | `openclaw security audit --deep` |

## Gateway Auth: Token (Recommended)

Token auth is recommended for headless/VM deployments:

```bash
openclaw onboard --gateway-auth token --gateway-token-ref-env OPENCLAW_GATEWAY_TOKEN
```

The token can come from:
- 1Password via op-forward exec provider (most secure)
- Environment variable
- Generated: `openclaw doctor --generate-gateway-token`

Gateway HTTP bearer auth is all-or-nothing operator access. Any credential that reaches the gateway has full access to chat, tools, and channels.

## AI Providers

Multiple providers can be configured simultaneously with fallback chains:

```
primary: anthropic/claude-opus-4-6
fallbacks: [anthropic/claude-sonnet-4-5, openai/gpt-5]
```

### Ollama (local models)

- Select Ollama during provider setup
- Base URL default: `http://127.0.0.1:11434`
- Two modes: Cloud + Local (ollama.com models + local) or Local only
- Auto-discovers available local models
- In cloister: Ollama runs on the host, accessed via tunnel to the VM

### 1Password / op-forward Integration

OpenClaw's SecretRef system supports exec providers:

```json
{
  "secrets": {
    "providers": {
      "op-anthropic": {
        "source": "exec",
        "command": "/usr/bin/op",
        "args": ["read", "--no-newline", "op://Vault/Item/field"],
        "passEnv": ["OP_SERVICE_ACCOUNT_TOKEN"]
      }
    }
  }
}
```

SecretRef resolution is eager at startup — gateway fails fast if any SecretRef can't resolve.

Known issue: onboarding crashes when using exec secret provider for channel tokens (openclaw/openclaw#37303). Use plaintext during onboard, then migrate to SecretRef afterward.

## Channels

Available: WhatsApp, Telegram, Slack, Discord, Google Chat, Signal, BlueBubbles (iMessage), IRC, Teams, Matrix, Feishu, LINE, Mattermost, and more.

Easiest to set up: Telegram, Discord (just a bot token).

iMessage requires macOS — which Lume provides natively.

## Daemon

`--install-daemon` creates `~/Library/LaunchAgents/com.openclaw.gateway.plist` on macOS. Auto-starts on boot, restarts on crash.

Store secrets in `~/.config/openclaw/secrets.env` with `chmod 600`, not in the plist.

## Skills

Skills are directories with `SKILL.md` + YAML frontmatter. Load priority:
1. Workspace skills (`~/.openclaw/workspace/skills/`)
2. Managed skills (`~/.openclaw/skills/`)
3. Bundled skills

ClawHub (clawhub.com) is the public skills registry.

## macOS App (Host-Side)

The OpenClaw macOS app can run on the host in **remote mode**, connecting to the gateway inside the VM via SSH tunnel:

```
Host: OpenClaw.app (remote mode) → SSH tunnel → VM: Gateway (loopback:18789)
```

The host app provides: menu bar UI, TCC permissions (camera, screen, notifications), native macOS integration. The VM gateway provides: AI, tools, sandboxed execution.

## What Cloister Should Automate vs Require User Input

### Can automate:
- Gateway bind: `loopback`
- Gateway port: `18789`
- Workspace directory: `~/.openclaw/workspace`
- Daemon installation
- Default skills
- Security audit run after setup

### Needs user input (first time):
- AI provider selection and API key (or 1Password vault reference)
- Channel configuration (bot tokens)
- Model preferences
- Ollama Cloud+Local vs Local-only choice
- Gateway auth token (generate or provide)

### Suggested cloister flow:
1. `cloister create --openclaw` installs OpenClaw, sets up daemon with gateway bind loopback
2. User runs `cloister agent forward <profile> 18789` to create SSH tunnel
3. User opens `http://localhost:18789` in host browser for first-time web UI setup
4. OR user runs `openclaw onboard` interactively inside the VM via `cloister exec`
5. `cloister repair <profile>` verifies all components are configured

## Sources

- https://docs.openclaw.ai/start/getting-started
- https://docs.openclaw.ai/gateway/security
- https://docs.openclaw.ai/gateway/authentication
- https://docs.openclaw.ai/gateway/secrets
- https://docs.openclaw.ai/cli/onboard
- https://docs.openclaw.ai/providers/ollama
- https://docs.openclaw.ai/platforms/macos
- https://docs.openclaw.ai/concepts/model-providers
- https://docs.openclaw.ai/tools/skills
