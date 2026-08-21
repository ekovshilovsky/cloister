<!-- Proprietary and confidential. All rights reserved. -->

# Tier 2 synchronized workspaces

Tier 2 broker mode is an opt-in project-scoped synchronized copy. It is not a
shared local filesystem and does not provide synchronous read-after-write
semantics. `flush` is the durability and visibility barrier.

Configure one profile project root:

```yaml
version: 3
profiles:
  work:
    backend: colima
    start_dir: ~/code/my-project
    workspace:
      mode: broker
      ignore:
        - .local-generated/
```

Omitted `workspace.mode` values migrate in memory to `virtiofs`, so existing
profiles retain their prior behavior. Loading an old file does not rewrite it.
The version 3 schema is written on the next authorized config save.

Broker mode requires Mutagen 0.18.1 and OpenSSH. Cloister detects both tools and
returns installation guidance when Mutagen is absent. Cloister never installs
Mutagen automatically because binary provenance and license approval are a
separate decision. The documented Homebrew command is:

```sh
brew install mutagen-io/mutagen/mutagen
```

Each profile and canonical project pair receives one stable Mutagen session and
one stable guest target under `~/workspaces/`. The session uses `two-way-safe`,
portable symlinks, an isolated Cloister Mutagen data directory, and a private
OpenSSH wrapper. VM starts use `BrokerWorkspace`, which means the project root
is not passed to Colima or Lume as a virtiofs mount. Supplemental mounts remain
separate.

Activation blocks until the initial flush completes and status reports no
conflicts or endpoint problems. Guest command execution, clean stop, rebuild,
reset, resize, and snapshots use the same barrier. Normal stop pauses the
session after a clean flush. Destructive rebuild and reset flows terminate the
old session only after the clean barrier, because the old guest copy is about
to be replaced. A failed flush or unresolved conflict refuses clean teardown
and leaves the VM running for recovery.

Interactive entry runs the guest login shell from the stable synchronized path.
When that shell exits, Cloister performs the same clean barrier and pauses the
session. A failed detach barrier returns an error and retains recoverable state.

Repository `.gitignore` files are compiled into one root-relative ordered
Mutagen policy. Unsupported rules fail activation with source and line details.
Profile ignores follow repository rules. Mandatory VCS, dependency, build,
output, cache, virtual environment, swap, and backup exclusions are applied
last and cannot be negated. `.git` remains host-side, so Git commands are not
available against the synchronized guest copy. Cloister prints a one-time
warning before interactive agent entry.

Preflight rejects hardlinked included files, escaping or absolute symlinks,
special files, and nested filesystems. Portable relative symlinks are accepted.
Material macOS extended attributes produce warnings because they remain
host-side.

## Verification boundary

The default unit suite uses `broker.Mock` and an injected Mutagen command runner.
It verifies lifecycle order, exact safe-mode configuration, final mandatory
ignores, Git ignore intent against `git check-ignore`, conflict refusal, stable
guest paths, preflight behavior, and that Cloister retains neither host file
descriptors nor per-file bookkeeping after scanning.

The following claims require a separate end-to-end run with Mutagen 0.18.1 and
a disposable running Colima or Lume VM:

- SSH agent injection and private wrapper interoperability for both backends.
- Host-to-guest and guest-to-host propagation, atomic saves, watcher events,
  executable-bit behavior, Unicode and case behavior, and conflict recovery.
- Pause, resume, VM restart, crash recovery, and snapshot behavior using real
  Mutagen three-way history.
- Host file descriptor measurements during initial scan, recursive guest reads,
  several concurrent projects, detach, and VM stop. The acceptance check is
  bounded growth that does not correlate with project entry count.

These end-to-end checks are intentionally not part of `go test ./...`; they
require an approved external binary and VM state.
