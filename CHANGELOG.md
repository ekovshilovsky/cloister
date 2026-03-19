# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.1] - 2026-03-19

### Added

- `cloister exec <profile> <command>` — run commands inside a VM without entering an interactive session. Useful for one-off administration, tool installation, and scripted operations.
- `~/code` symlink inside VMs alongside `~/workspace` for natural navigation.

### Fixed

- Switched to native Claude Code installer (`claude.ai/install.sh`) instead of the deprecated npm package.
- Installed `zstd` before the Ollama installer, which requires it for archive extraction.

## [0.1.0] - 2026-03-19

### Added

- **Ollama stack**: new `ollama` provisioning stack installs the Ollama CLI inside VMs with the local server disabled. Inference runs on the host's Metal GPU via an SSH reverse tunnel (port 11434). The host's model cache (`~/.ollama/models`) is mounted read-only into the VM for zero-duplication model access.
- **Tunnel consent system**: per-profile `tunnel_policy` field controls which host services are forwarded into the VM. Values: `auto` (default for interactive — forward all detected services), `none` (default for headless — forward nothing), or an explicit whitelist (e.g., `[clipboard, ollama]`).
- **Mount consent system**: per-profile `mount_policy` field controls which host directories are mounted. Same semantics as tunnel policy. Headless profiles default to workspace + Claude extensions only, with Claude extension directories mounted read-only to prevent lateral movement attacks.
- **Configurable workspace mount**: `start_dir` is now the single source of truth for the workspace directory — it's both what gets mounted and where the shell starts. Users whose code lives outside `~/code` can specify any absolute path (e.g., `~/Projects/my-app`). Interactive wizard detects `~/code` and prompts if missing.
- **Tunnel and mount name validation**: unknown names in explicit policy lists are rejected at create time with a list of valid options.
- **Ollama builtin tunnel**: auto-discovers host Ollama on port 11434 (TCP health check) and tunnels it into the VM alongside clipboard, 1Password, and audio.
- **Host detection warning**: after provisioning the ollama stack, cloister probes the host for a running Ollama server and prints guidance if not detected.
- **add-stack restart flow**: when adding a stack that requires new mounts (e.g., `ollama`), cloister detects the mount change and prompts for a VM restart.
- **Semver dev builds**: `make build` now derives version from git tags (e.g., `0.0.2-dev.25+abc1234`). Tagged releases produce clean versions (e.g., `0.1.0`).

### Changed

- **~/Code → ~/code**: all references to the default workspace path standardized to lowercase.
- **Unified CI build system**: Makefile is now the single source of truth for version derivation and build configuration. CI workflows (`ci.yml`, `release.yml`) call `make` targets instead of reimplementing build logic.
- **ValidateStacks error messages**: now generated dynamically from the valid stacks map instead of hardcoded strings.
- **read-only-mounts.sh**: extended to enforce read-only on `~/.ollama/models`. For headless profiles, also enforces read-only on Claude extension directories (`~/.claude/plugins`, `skills`, `agents`).

### Fixed

- **Workspace validation before config save**: the profile is now validated before being persisted, preventing broken entries from remaining in config if the workspace path doesn't exist.
- **Relative path rejection**: `ResolveWorkspaceDir` rejects relative paths and `~user` syntax with clear error messages instead of silently producing invalid mount paths.
- **IPv6-safe address formatting**: `checkHost` uses `net.JoinHostPort` instead of `fmt.Sprintf` for correct IPv6 address handling.
- **zstd dependency for Ollama installer**: `stack-ollama.sh` installs `zstd` before running the Ollama installer, which requires it for archive extraction.

### Security

- Headless profiles default to `tunnel_policy: none` and `mount_policy: [code, claude-plugins, claude-skills, claude-agents]` — no SSH keys, GPG keys, or Downloads exposed unless explicitly configured.
- Claude extension directories (`~/.claude/plugins`, `skills`, `agents`) are read-only in headless profiles to prevent a compromised agent from writing malicious extensions that would be loaded by interactive profiles.
- `os.Stat` workspace validation catches permission errors (not just `IsNotExist`), surfacing inaccessible directories early.

## [0.0.2] - 2026-03-17

### Changed

- Updated README title with SEO keywords and OpenClaw reference for agent sandboxing.
- Linked PulseAudio to GitHub repo in README.

## [0.0.1] - 2026-03-17

### Added

- Core CLI with Cobra command framework
- Profile management: `create`, `stop`, `delete`, `status` commands
- Interactive creation wizard with defaults-first prompt
- Non-interactive mode via flags (`--defaults`, `--memory`, `--stack`, etc.)
- AI-friendly discovery: `--list-options --json` for machine-readable option schemas
- Colima VM lifecycle management (invisible to user)
- Config system with YAML persistence at `~/.cloister/config.yaml`
- Memory budget manager with auto-calculated limits and idle-based eviction suggestions
- Tunnel auto-discovery for clipboard (cc-clip), 1Password (op-forward), and audio (PulseAudio)
- SSH reverse port forwarding for all detected host services
- Guided setup command for optional services (`cloister setup op-forward`, `cloister setup audio`)
- Base provisioning: git, Node.js LTS, pnpm, Claude Code, GPG tools, direnv
- Seven composable provisioning stacks: `web`, `cloud`, `dotnet`, `python`, `go`, `rust`, `data`
- Stack version configuration (`--dotnet-version`, `--python-version`, `--go-version`, etc.)
- `add-stack` command for incremental stack installation
- Custom provisioning hooks (`~/.cloister/provision.sh`, `~/.cloister/provision-<profile>.sh`)
- Backup/restore/rebuild cycle for session data preservation across VM rebuilds
- iTerm2 integration: background colors and tab/window titles per profile
- Fallback terminal identity banner for non-iTerm terminals
- Self-update command with GitHub Releases download
- CI/CD: GitHub Actions for test/vet on push, tag-driven releases with Homebrew tap auto-update
- Pre-commit hooks for formatting, vetting, and test validation
- NOTICE file documenting third-party dependency licenses

### Security

- VM-level isolation (Apple Virtualization Framework, not containers)
- Read-only enforcement for SSH keys and GPG keys via post-boot remount
- Loopback-only tunnel binding — host services unreachable from network
- GPG key material excluded from backups

[0.1.1]: https://github.com/ekovshilovsky/cloister/releases/tag/v0.1.1
[0.1.0]: https://github.com/ekovshilovsky/cloister/releases/tag/v0.1.0
[0.0.2]: https://github.com/ekovshilovsky/cloister/releases/tag/v0.0.2
[0.0.1]: https://github.com/ekovshilovsky/cloister/releases/tag/v0.0.1
