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
one managed guest target under `~/workspaces/`. Mutagen session identity remains
`cloister-<profile>-<project-id>` (hash-based and unchanged across layout
migrations). The on-disk guest path is layout-driven for `workspace` mode:

```yaml
workspace:
  mode: workspace
  root: ~/code/company
  selectors: [apps/*, tools/*]
  layout:
    scheme: mirror          # default in workspace mode; guest paths follow selectors
    group_by_org: auto      # prefix <org>/ when the collection spans multiple GitHub orgs
    # template: reserved for a future custom scheme
```

`scheme: mirror` places `apps/api` at `~/workspaces/apps/api` instead of a
hashed basename such as `~/workspaces/api-f3870c0999f1`. `scheme: flat` keeps
that legacy basename-plus-hash path. `group_by_org` is `auto` (default),
`"true"`, or `"false"`. Auto prefixes `<org>/` only when the selected set has
more than one distinct non-empty GitHub org, parsed from a manifest `repo` URL
or the host-side `origin` remote. Single-project `mode: broker` profiles keep
the hashed guest path unless a layout is set.

If two projects would land on the same guest path, discovery fails and names
both selectors. Existing sessions whose Mutagen beta path differs from the
desired GuestRoot are terminated and recreated at the new target rather than
syncing into the stale directory.

The session uses `two-way-safe`, portable symlinks, an isolated Cloister Mutagen
data directory, and a private OpenSSH wrapper. VM starts use `BrokerWorkspace`,
which means the project root is not passed to Colima or Lume as a virtiofs
mount. Supplemental mounts remain separate.

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
workspace configuration before broker activation. Review and apply are
interactive by default. `workspace review` accepts `--accept-recommendations`,
class, path, and project decision flags, `--exclude-unresolved`, and `--yes`.
`workspace apply --yes` writes the printed delta without a confirmation prompt.
Without `manifest/projects.json`, scan walks the source root for repository boundaries
instead of treating glob-matched directories as projects. A directory is a
canonical repository when its exact `.git` child is a real directory, and it is
a worktree checkout when `.git` is a regular file. The pointer file is never
read. `.git` symlinks are not followed, and every child is checked with `Lstat`
immediately before descent. The scanner owns the shared list of dependency,
generated, credential, private, and cache directory names that every walk
prunes. That list includes `.terraform` and `.terragrunt-cache`. Repository
boundary discovery additionally prunes `bin` and `obj`, while the scanner keeps
those names traversable because they can contain source outside known build
configurations. It also prunes `.agent-grid` as host-private agent runtime
state, `.turbo` as rebuildable cache state, and `.playwright-data` plus
`.playwright-data-*` as machine-local browser profile state. `vendor` remains
traversable.

Discovery continues below repository roots so nested repositories are separate
candidates. A repository that contains nested repositories defaults to review
with its nested count and an overlap warning. Leaf repositories and worktree
checkouts default to include. Existing selectors remain proposal provenance but
do not override candidate defaults or hide a newly created repository or
worktree. Review excludes an included nested repository when its parent is also
included. Apply still rejects an included parent and child pair left in saved
state and tells the user to keep exactly one. For each included repository, apply also adds every nested
repository candidate as a parent-relative directory ignore. This rule is
independent of whether the nested candidate is included or excluded, so a
parent session never synchronizes a nested repository tree.
The `.` selector is accepted only as the sole selector and only when the source
root has a non-symlink `.git` directory or regular worktree pointer file.
Combining the root with child selectors is rejected. When the source root is
the only repository, scan recommends single-project `mode: broker` instead of
`mode: workspace`.

The repository walk defaults to a maximum depth of 64 directories below the
source root, at most 100,000 visited directories, and at most 10,000 discovered
repository roots. The walk reads directory entries only to find subdirectories
to descend into, so a directory holding a very large number of plain files is
traversed without consuming a bound. Directory entries are read in bounded
batches of 256, so a very wide directory never enters memory whole. Exceeding
any walk bound aborts discovery without truncating the result. Proposal schema
version 2 records repository candidates and their decisions. Each project also
records `incompleteScan` and an actionable `scanIssue` when its entry or byte
bound is exceeded. Scan continues through the remaining projects, but apply
refuses to include incomplete projects. The project must be excluded or
narrowed with global or per-project ignores and scanned again. Configured
ignored entries do not count toward entry or byte bounds. Ignored directories
are pruned unless a later negation may re-include a descendant, in which case
the scanner descends and reviews the re-included entries. Local state format
version 2 stores a `contentFingerprint` alongside the config and source
fingerprints. It is derived from sorted project identity (ID, portable path, and
kind) and bounded project-tree metadata for every scanned entry, including
classifier-pruned directory entries but excluding configured ignored entries.
Each entry contributes its project-relative path, mode type, and permission
bits. A regular file also contributes only whether its reported size meets the
same large-file threshold used by classification. Directories contribute no
size. Exact file size and modification time are excluded because they do not
change a classification decision. The fingerprint detects changes that can
alter review or introduce an unreviewed path, not every write. File contents are
not read, and the fingerprint never enters portable proposal JSON. Configuration
and source fingerprints bind the ignore policy. The state also requires a
per-project fingerprint map for stale diagnostics.

Cookie stores are credential-equivalent metadata-only findings that default to
`secret_local_config`/review and are never opened. The rule matches exact
case-insensitive basenames `cookies`, `cookies-journal`,
`safe browsing cookies`, `safe browsing cookies-journal`, `cookies.sqlite`,
`cookies.txt`, and `cookies.json`, plus files with the exact `.cookies`
extension. It does not match code such as `utils/cookies.ts`, a
`cookie-policy` page, or a `.cookie` cache marker.

State format version 1 is not reused because it cannot represent the new
project candidate model. Loading it tells the user to re-run
`cloister workspace scan`.

Review and apply recompute the same bounded fingerprint with the same project
validation, symlink behavior, prune rules, and entry and byte caps. Added,
removed, renamed, retyped, or permission-changed entries, a regular file
crossing the large-file threshold, and a newly added private path make the saved
scan stale. A content, exact-size, or modification-time change within one size
bucket does not. A stale error names at most five changed portable project
paths, includes a count of any remainder, never prints an absolute host path,
and tells the user to re-run the scan. The stale check occurs before a review
state save or workspace config write. Show uses the same freshness gate.
`cloister workspace show --allow-stale` permits explicit inspection of stale
saved state and prints a warning first.
The scanner excludes an entry named exactly `.git` whether it is a directory or
a regular worktree pointer file. It prunes the directory and never opens the
file. `.gitignore`, `.gitattributes`, and `.github` remain ordinary source.

## Verification boundary

The default unit suite uses `broker.Mock` and an injected Mutagen command runner.
It verifies lifecycle order, exact safe-mode configuration, final mandatory
ignores, Git ignore intent against `git check-ignore`, conflict refusal, stable
guest paths, preflight behavior, and that Cloister retains neither host file
descriptors nor per-file bookkeeping after scanning. Workspace discovery tests
also verify stable fingerprints for ordinary writes, fingerprint drift for
added, removed, retyped, large-threshold-crossing, and newly private entries,
and bounded portable per-project stale diagnostics, with no stale state or
config write.

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
