# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

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

[0.0.1]: https://github.com/ekovshilovsky/cloister/releases/tag/v0.0.1
