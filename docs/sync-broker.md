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

Activating a complete `workspace` collection also reconciles the isolated
Mutagen session inventory before any desired session is created or resumed.
Cloister compares only exact, sanitized `cloister-<profile>-<project-id>`
session identities for that profile. Desired sessions, another profile's
sessions, and sessions not managed by Cloister are preserved. This avoids stale
or duplicate synchronization when a profile migrates from one broker project
to a pinned multi-project collection, or when collection selectors remove a
project.

Obsolete active sessions are flushed, re-read, required to be clean, and then
terminated. Obsolete paused sessions must already report clean before
termination. Missing sessions are treated as already gone. An ambiguous session
list, malformed Cloister identity, conflict, endpoint problem, unknown state,
flush failure, or termination failure stops collection activation before any
desired session is touched. A successful termination removes the existing
policy fingerprint through the normal termination path.

Single-project broker activation and path-specific `cloister open` do not
reconcile the profile inventory. They cannot prove that sibling collection
sessions are obsolete. To recover from a failed collection reconciliation,
inspect the reported session, resolve conflicts or endpoint connectivity, and
activate the complete collection again. For an already running profile, use
`cloister exec <profile> -- true`. For a stopped profile, use
`cloister <profile>`. Do not delete policy fingerprints or Mutagen state by
hand.

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

## Scoped workspace discovery

`cloister workspace scan`, `review`, and `apply` can build a pinned multi-project
workspace configuration before broker activation. Local state format version 1
stores a `contentFingerprint` alongside the config and source fingerprints. It
is derived from sorted project identity and bounded project-tree metadata:
project-relative path, type and mode, reported size, and modification time for
every visited entry, including pruned directory entries. File contents are not
read, and the fingerprint never enters portable proposal JSON.

Review and apply recompute the same bounded fingerprint with the same project
validation, symlink behavior, prune rules, and entry and byte caps. Added,
removed, renamed, resized, or retimestamped entries, including a newly added
private path, make the saved scan stale. Recovery is to scan and review again.
The stale check occurs before a review state save or workspace config write.

## Verification boundary

The default unit suite uses `broker.Mock` and an injected Mutagen command runner.
It verifies lifecycle order, exact safe-mode configuration, final mandatory
ignores, Git ignore intent against `git check-ignore`, conflict refusal, stable
guest paths, preflight behavior, and that Cloister retains neither host file
descriptors nor per-file bookkeeping after scanning. Workspace discovery tests
also verify content fingerprint drift for added, removed, renamed, size-only,
mtime-only, and newly private entries, with no stale state or config write.

For collection reconciliation, verify:

- An active obsolete session is flushed, re-read, and terminated in order.
- A clean paused obsolete session is terminated without a flush.
- Desired sessions, other profiles, non-Cloister sessions, and profile names
  that merely share a prefix are preserved.
- Empty inventories are a no-op.
- Conflict, endpoint, parse, flush, and termination failures prevent desired
  session creation or resume.
- Complete collection activation reconciles before create, while
  single-project and path-specific activation do not reconcile.

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

A 2026-08-25 running collection check covered that remaining boundary. Manifest
scan, sectioned review, and apply produced a pinned 8-project workspace.
Activation left exactly 8 desired sessions after conservative obsolete-session
reconciliation. Configured exclusions were absent in the guest. Repository
instructions and SQL source were present. Recursive guest reads of project
source succeeded. Host Git and `gh` proxying worked from a guest project. Host
file descriptors moved from 20,191 of 491,520 to 21,327 of 491,520, about
4.34% of the limit, with no whole-tree mount exhaustion.
