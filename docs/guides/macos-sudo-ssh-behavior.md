# macOS sudo Behavior: SSH vs Terminal

Research conducted 2026-03-25 for cloister's Lume VM provisioning.

## Summary

`sudo` with NOPASSWD works identically over SSH (no TTY) and in Terminal.app (with TTY) on macOS. There is no macOS-specific barrier, no `requiretty` default, and no PAM module that discriminates against SSH sessions when NOPASSWD is configured.

## Key Findings

### No TTY requirement on macOS

macOS has never shipped with `Defaults requiretty` in `/etc/sudoers`. This is a RHEL/CentOS-specific default that does not exist on macOS. When NOPASSWD is configured, `sudo -n` works regardless of TTY availability.

### PAM chain is irrelevant for NOPASSWD

macOS's `/etc/pam.d/sudo` includes `pam_tid.so` (Touch ID) and `pam_smartcard.so`, which only work in local Terminal sessions. Over SSH these are skipped, falling through to `pam_opendirectory.so` (password auth). However, when NOPASSWD is configured, the entire PAM chain is bypassed — sudo never calls into PAM for authentication.

### `echo password | sudo -S` has stderr mixing issues

`sudo -S` writes its password prompt (`Password:`) to stderr. When `lume ssh` merges stderr into stdout (NIO SSH client uses one buffer), the prompt text contaminates command output. This causes `grep` checks to behave unpredictably.

**Fix:** Only use `echo lume | sudo -S` for the initial sudoers file creation (before NOPASSWD exists). All subsequent commands should use `sudo -n` which produces no stderr noise.

### sudoers.d file requirements on macOS

| Attribute | Required Value |
|-----------|---------------|
| Mode | `0440` (`-r--r-----`) |
| Owner | `root` (uid 0) |
| Group | `wheel` (gid 0) |
| Filename | No dots (`.`) in filename — files with dots are silently skipped |

macOS default sudoers includes `#includedir /private/etc/sudoers.d`. The `#` is directive syntax, not a comment. `/etc` symlinks to `/private/etc`, so both paths work.

### sudo re-reads config on every invocation

There is no daemon, no cache, no reload needed. Writing a file to `/etc/sudoers.d/` makes it effective immediately for the next `sudo` invocation, even within the same SSH session.

### Rule ordering: last match wins

If multiple sudoers entries match a user, the last one wins. Files in `sudoers.d/` are read in lexical order after the main sudoers file. A NOPASSWD entry in `sudoers.d/lume` will override any password-requiring entry in the main file because it's read last.

## Correct Patterns for Cloister

### In YAML preset (post_ssh_commands run by Lume)

Lume's `HealthCheckRunner.runPostSshCommands()` auto-rewrites every occurrence of `"sudo "` to `"echo 'lume' | sudo -S "`. This happens unconditionally — it does NOT skip commands that already contain `sudo -S`. Therefore:

- Use bare `sudo` in post_ssh_commands — Lume adds the password pipe
- Do NOT write `echo lume | sudo -S` in the YAML — it gets double-rewritten

```yaml
post_ssh_commands:
  - "sudo sh -c 'echo \"lume ALL=(ALL) NOPASSWD: ALL\" > /etc/sudoers.d/lume'"   # Lume rewrites to echo|sudo -S
  - "sudo pmset -a displaysleep 0 sleep 0"                                         # Lume rewrites to echo|sudo -S
  - "defaults -currentHost write com.apple.screensaver idleTime -int 0"            # No sudo needed
```

### In repair (commands run via `lume ssh`)

`lume ssh vmname -- "command"` does NOT rewrite sudo. Commands must handle auth explicitly:

```
Step 1 (bootstrap): echo lume | sudo -S sh -c '...'   (NOPASSWD may not exist yet)
Step 2+: sudo -n <command>                              (NOPASSWD is active, no password needed)
```

### In provisioner (commands run via cloister's SSH with key auth)

Same as repair — no Lume rewriting. Use `sudo -n` after NOPASSWD is set up.

### Checks should use `sudo -n`:

```
sudo -n cat /etc/sudoers.d/lume 2>/dev/null | grep -q NOPASSWD && echo OK || echo MISSING
```

### Preference persistence: use `-currentHost` for ByHost plists

macOS resets per-user screensaver defaults on reboot unless written to the ByHost plist:

```bash
defaults -currentHost write com.apple.screensaver idleTime -int 0           # Persists
defaults -currentHost write com.apple.screensaver askForPassword -int 0     # Persists
defaults write com.apple.screensaver askForPassword -int 0                  # Does NOT persist
```

### Power management: combine into one call

```bash
sudo pmset -a displaysleep 0 sleep 0    # Persists — both values in one call
```

## What NOT To Do

- Do not use `echo lume | sudo -S` in YAML post_ssh_commands — Lume auto-rewrites sudo and it gets double-piped
- Do not use `echo lume | sudo -S` for every command in repair — only for the bootstrap sudoers step; use `sudo -n` after
- Do not use `sudo -v` or `sudo -S -v` — the validate flag behaves differently from command execution
- Do not use `defaults write` (without `-currentHost`) for screensaver settings — they don't persist across reboot
- Do not assume `lume ssh` allocates a TTY for non-interactive commands — it does not
- Do not create LaunchDaemons to re-apply settings — macOS persists settings correctly with proper flags/domains

## Sources

- Apple open source: sudo ttyname.c, ttyname_dev.c
- sudo-project/sudo issues #258, #329, #344
- macOS 14.2.1 default sudoers (gist.github.com/keith/9061156)
- nix-darwin issue #784 (macOS Sonoma PAM changes)
- sudo.ws manual (sudoers, requiretty, use_pty)
