---
documentVersion: 1
status: release-ready, verified 2026-08-25
date: 2026-08-25
---

# Scoped workspace discovery

## Purpose

Scoped workspace discovery turns an existing local project catalog into a
reviewed Cloister workspace configuration. Its contract is:

```text
scan -> review -> explicit local apply
```

Scanning records evidence and recommendations. Review resolves every uncertain
finding. Apply shows an exact field-level delta and writes only after a separate
confirmation. No stage starts or modifies a VM.

This document defines schema version 1, local state format version 1, the local
CLI workflow, and the safety boundary. The v1 contract is verified and
release-ready. It is not a published release.

## User contract

### Scan

`cloister workspace scan <profile|path>` resolves a configured profile and
loads one project source:

1. The generic selector adapter reuses the configured workspace root,
   selectors, project-specific ignores, and existing selector validation.
2. The workspace-manifest adapter loads the canonical project catalog and its
   optional local metadata overlay. Worktree sets are excluded unless explicitly
   requested by the adapter.
3. The bounded scanner walks only validated project roots, classifies entries
   from filesystem metadata, opens only allowlisted safe manifests whose final
   classification is a manifest class, and produces a project-tree content
   fingerprint during the same traversal.
4. Cloister saves a private local envelope containing the portable proposal,
   physical project mappings, and freshness fingerprints.

The path form selects the most specific configured profile whose workspace
source contains that path. No match and equally specific matches fail with a
diagnostic instead of guessing.

`--json` prints only the portable proposal. It does not expose local physical
roots or local fingerprints. The scan is still saved for review.

### Review

`cloister workspace review <profile>` first verifies that the saved scan still
matches the relevant profile configuration, source catalog, local project
mappings, and project-tree metadata. It then groups findings into these
sections:

1. Environment and secret-like local configuration
2. Dependencies
3. Generated artifacts
4. Local databases, dumps, and scripts
5. Application commands and services
6. Repository-owned agent configuration
7. Unknown large files
8. Other source and local inputs

Entries classified as `review` require an explicit include or exclude decision.
When a section has multiple unresolved entries, `include-all` (`ia`) and
`exclude-all` (`ea`) explicitly apply that decision to the current entry and
all remaining unresolved entries in that section only. Bulk decisions never
cross section boundaries. After all decisions are resolved, the user confirms
whether to save the reviewed proposal. Cancellation, invalid input, or EOF
leaves the prior state unchanged.

`cloister workspace show <profile>` displays the saved state in sections.
`cloister workspace show <profile> --json` emits only proposal schema v1.

### Apply

`cloister workspace apply <profile>` accepts only a fresh, fully reviewed local
state. Apply:

1. Rejects external, changed, or otherwise unrepresentable physical mappings.
2. Pins `workspace.selectors` to the exact reviewed project paths. Globs are not
   carried into the applied selection.
3. Preserves global workspace ignores from the proposal.
4. Builds `workspace.project_ignore` with exact project path keys and exact
   excluded finding paths. Excluded directories receive a trailing slash.
5. Carries the reviewed entry cap and staging file size into the local workspace
   fields.
6. Prints a field-level delta for mode, root, selectors, ignore rules,
   project-specific ignores, entry cap, and staging file size.
7. Requires a separate `yes` confirmation before calling the existing config
   save path.

Only the selected profile's `workspace` field changes. Unrelated profiles,
global configuration, and config schema version 4 remain unchanged.

## Architecture

### Project source adapters

The source boundary is a small interface that returns portable project
descriptors, policy metadata, canonical local roots, approved external roots,
and a metadata digest.

The generic selector adapter is the default when no workspace manifest exists.
It delegates root and selector semantics to the same workspace selection code
used for activation. Default selectors remain `apps/*` and `tools/*` when the
profile has none.

The workspace-manifest adapter reads only its canonical catalog metadata and an
optional local metadata overlay. Before any open, it requires
`manifest/projects.json` and any present `.workspace.local.json` to be
non-symlink regular files. It validates format versions, rejects unknown JSON
fields, resolves project and optional worktree catalogs, rejects duplicate or
nested physical roots, and emits deterministic descriptors. External catalog
roots must be inside an explicitly approved root. These external mappings can be
scanned and represented portably, but local config apply rejects them because
local selectors cannot represent them safely.

### Bounded metadata scanner

The scanner receives validated project descriptors rather than discovering
arbitrary directories. It counts every walked entry and the reported size of
every regular file. Default per-project caps are:

| Limit | Default |
|---|---:|
| Entries | 100,000 |
| Reported regular-file bytes | 4 GiB |
| Unknown large-file threshold | 50 MiB |
| Safe manifest parse size | 1 MiB |

A profile or source policy can lower or explicitly set applicable project caps.
Crossing an entry or byte cap aborts the scan with the project identifier and
limit kind. Cloister never widens a cap automatically.

Secret-like, credential, certificate, and machine-local configuration files are
metadata-only findings. Their contents are never opened. Clearly named
development templates such as `.env.example`, `.env.sample`, `.env.template`,
`.env.example.backup`, and `appsettings.Local.example.json` default to
source/include, but remain metadata-only unless their basename is also on the
safe manifest allowlist. Actual `.env`, `.env.local`,
`appsettings.Local.json`, `.npmrc`, credentials, keys, and certificates retain
their review or exclude safety defaults. The scanner may open only this
allowlist to derive non-secret runtime, command, and service metadata:

- `package.json`
- `go.mod`
- `global.json`
- `compose.yaml`
- `compose.yml`
- The two legacy compose filename variants recognized by the scanner

Manifest reads are capped at 1 MiB. Script bodies and other command content are
not copied into the proposal. Only command names are retained.

Pruning is conservative. Clearly rebuildable dependency and cache directories,
generated artifact directories, repository metadata, high-confidence
credential directories such as `.ssh`, `.aws`, and `.gnupg`, and clearly
host-private agent state directories are pruned. Generated .NET configuration
subtrees are pruned only after reaching `bin/Debug`, `bin/Release`, `obj/Debug`,
or `obj/Release`, matched case-insensitively. A `bin` or `obj` directory itself
remains traversable, so arbitrary source beneath other subdirectories stays
visible. High-confidence backup or dump SQL defaults to
`database_dump`/exclude. Every other `.sql` file is a source or development
script and defaults to `database_script`/include. SQL is never manifest-parsed.
Repository-owned instructions and unknown large files remain visible for
review. Symlinked project roots are rejected, nested symlinks are not followed,
and canonical containment is checked before scanning.

The scanner also hashes sorted project identity and metadata for every visited
entry, including each pruned directory entry. The metadata record contains only
the project-relative path, type and mode, reported size, and modification time.
It does not read file contents. The same entry and byte caps, project
validation, symlink behavior, and prune rules apply when review or apply
recomputes the fingerprint.

Errors exposed by scanner and manifest boundaries omit absolute local paths and
file contents where those details are not needed for recovery.

### Proposal schema v1

The proposal is the portable, shareable review artifact. It contains no absolute
local roots. Collections are normalized for deterministic JSON output.

| Field | Meaning |
|---|---|
| `schemaVersion` | Required schema discriminator. Version 1 only. |
| `createdAt` | UTC creation timestamp. |
| `generator` | Cloister generator version. |
| `source.root` | Always the portable relative root `"."`. |
| `source.adapter` | `generic` or `workspace_manifest`. |
| `projects[]` | Stable `id`, portable `path`, and `kind`. |
| `findings[]` | Project-relative path, type, size, reason, recommendation, and reviewed decision. |
| `runtimes[]` | Runtime name, optional version, project, and evidence path. |
| `commands[]` | Command name, project, and safe manifest path. |
| `services[]` | Service name, project, and safe manifest path. |
| `policy.selectors` | Source selectors retained as scan provenance. |
| `policy.ignore` | Workspace-wide ignore rules. |
| `policy.projectIgnore` | Exact portable project paths mapped to ignore rules. |
| `policy.maxStagingFileSize` | Optional synchronization staging limit. |
| `policy.maxEntriesPerProject` | Positive per-project entry cap. |
| `policy.maxBytesPerProject` | Positive per-project reported-byte cap. |
| `policy.prunePatterns` | Directories pruned by the scan policy. |
| `exclusions[]` | Exact normalized projection of findings whose final decision is `exclude`, preserving project, path, class, and reason. |
| `cloudReadiness` | Always `local_only` in v1. |
| `unansweredCloudQuestions[]` | Explicit future portability questions. |

Project paths and evidence paths are clean relative slash paths. References must
name existing project identifiers. Required collections must be present, even
when empty. Duplicate identifiers and paths are rejected. Unknown fields and
unsupported schema versions fail closed when loaded from local state.

### Local state format v1

The local envelope is private machine state, not a portable interchange file.
It contains:

| Field | Meaning |
|---|---|
| `formatVersion` | Local state format discriminator. Version 1 only. |
| `profile` | Safe local profile identifier. |
| `sourceRoot` | Canonical absolute source root. |
| `configFingerprint` | Digest of the profile fields relevant to discovery. |
| `sourceFingerprint` | Digest of adapter, projects, policy, approved roots, and catalog metadata. |
| `contentFingerprint` | Digest of sorted project identity and bounded project-tree metadata from the scanner traversal. |
| `proposalDigest` | Digest of normalized proposal schema v1. |
| `reviewed` | True only after all review decisions and final save confirmation. |
| `projectMappings[]` | Project ID and portable path mapped to a canonical physical root. |
| `proposal` | The complete portable proposal. |

The state directory is private, newly created parent directories use mode
`0700`, and state files use mode `0600`. Saves write and sync a temporary file,
then atomically rename it over the destination. Failed writes remove the
temporary file.

The state and proposal loaders have explicit migration registries. Version 1 has
no predecessor, so both registries are empty. A future migration must be
registered by source version, validate the migrated result, preserve the
original file, and never silently rewrite during load. Newer versions and older
versions without a registered migration fail closed.

The normalized proposal digest detects proposal tampering. Configuration,
source, and content fingerprints detect stale scans. Fresh project mappings are
compared to the saved mappings to detect moved, replaced, or redirected roots.
The content fingerprint remains local-only and never enters portable proposal
JSON.

## Safety and failure behavior

Discovery and apply obey these defaults:

- No VM start, stop, mount, synchronization, provisioning, or other lifecycle
  side effect occurs.
- Project roots must be canonical real directories. Symlink escapes, duplicate
  physical roots, nested roots, and unapproved external roots are rejected.
- Secret-like and local configuration candidates are never opened. Credential
  and host-private directories are classified and pruned before descent, even
  when a child basename would otherwise be an allowlisted manifest.
- Safe configuration templates and non-dump SQL default include, remain
  metadata-only, and are never added to the manifest parsing allowlist.
- Only allowlisted safe manifests are parsed, with strict size limits.
- Unknown versions, unknown state fields, missing required fields, and malformed
  JSON fail closed.
- Review and apply both require fresh config, catalog, mapping, and project-tree
  content checks.
- Apply accepts only mappings that equal `sourceRoot/project.path`.
- Cancellation or EOF during review or apply performs no write.
- Bulk review is explicit and limited to remaining unresolved findings in the
  current section.
- Existing config save rotation and validation remain the persistence boundary.

Representative recovery messages are:

```text
workspace scan is stale because relevant profile configuration changed
workspace scan is stale because source catalog metadata changed
workspace scan is stale or tampered because project mappings changed
workspace scan is stale because the project tree changed
state proposal digest mismatch
workspace proposal has not been reviewed
workspace proposal has unresolved review decisions
project "<id>" uses an external or stale source mapping that local workspace selectors cannot represent safely
review not saved
workspace apply cancelled
```

After profile configuration, catalog metadata, or project-tree metadata
changes, run a new scan and repeat review. This includes added, removed,
renamed, resized, or retimestamped entries and newly created private paths.
After a mapping or proposal integrity failure, inspect the local workspace roots
and rescan rather than editing state by hand. An external mapping can still be
represented in a portable proposal, but it cannot be applied to local selector
configuration.

The first complete collection activation after apply reconciles obsolete
profile-owned Cloister synchronization sessions before creating or resuming any
desired session. This is the migration boundary for a profile that previously
used one broker project, as well as for later selector removals. Reconciliation
matches the exact sanitized profile identity and valid project identifier, so
another profile, a profile whose name merely shares a prefix, and non-Cloister
sessions remain untouched.

Reconciliation is conservative. Active obsolete sessions require a successful
flush and a clean status re-read. Paused obsolete sessions require a clean
status. Only then does the normal termination path remove the session and its
policy fingerprint. Missing sessions are already gone. Inventory ambiguity,
malformed Cloister names, conflicts, endpoint problems, unknown states, and
flush or termination failures stop activation before desired sessions begin.
Resolve the reported Mutagen health or inventory issue and activate the complete
collection again. Path-specific `cloister open` and legacy single-project
activation intentionally skip reconciliation because they do not describe the
complete desired profile inventory.

## Verification checklist

- Confirm allowlisted basenames below credential and host-private directories
  never reach the injected opener.
- Confirm canonical and optional local metadata symlinks are rejected before
  the injected opener runs.
- Confirm exclusions exactly match final exclude decisions after scan, review,
  state save, and state load.
- Confirm added, removed, renamed, resized, and retimestamped entries, plus a
  newly added private path, make review and apply reject stale state before any
  state or config write.
- Confirm portable proposal JSON contains no content fingerprint, file content,
  secret value, or absolute host path.
- Confirm complete collection activation reconciles before the first desired
  create or resume, including when migrating from one broker project.
- Confirm desired sessions, other profiles, non-Cloister sessions, and
  profile-prefix collisions are preserved.
- Confirm active obsolete sessions use flush, status, and termination order,
  while clean paused obsolete sessions terminate without a flush.
- Confirm empty inventories are a no-op and inventory, conflict, endpoint,
  flush, or termination failures prevent desired activation.
- Confirm path-specific open and legacy single-project activation do not
  reconcile sibling sessions.

## Command examples

The examples use neutral local names and paths.

```bash
# Scan by configured profile.
cloister workspace scan local-dev

# Scan by a project path inside a configured workspace scope.
cloister workspace scan ~/workspaces/apps/api

# Inspect portable output.
cloister workspace show local-dev --json

# Resolve all review decisions and confirm saving them.
cloister workspace review local-dev

# Inspect the exact workspace field delta, then confirm local apply.
cloister workspace apply local-dev
```

An applied workspace uses pinned selectors and exact project ignore keys:

```yaml
version: 4
profiles:
  local-dev:
    start_dir: ~/workspaces
    workspace:
      mode: workspace
      root: ~/workspaces
      selectors:
        - apps/api
        - tools/cli
      project_ignore:
        apps/api:
          - .env.local
        tools/cli:
          - build-cache/
      max_entry_count: 100000
```

## Future remote-container target

A later remote-container target shapes the portable proposal boundary, but is
not part of v1 behavior. Version 1 performs no upload, billing, remote resource
creation, provisioning, secret copying, or remote Git authentication.

Questions about future transport, storage, runtime construction, credentials,
networking, quotas, and teardown belong in `unansweredCloudQuestions`.
`cloudReadiness` remains `local_only` until a separately reviewed schema and
safety contract define another state. Local apply must not infer remote
readiness from an empty questions list.

## Final verification record

Verified 2026-08-25. Automated checks that passed: `make test`, `make vet`,
`make build`, gofmt, and `git diff --check`. Whole-feature review found no
Critical or Important defects. A security review finding was fixed and
re-verified.

A real running multi-project end-to-end check used the workspace-manifest
adapter, sectioned review, and local apply, then activated the resulting
collection:

- Scan selected 8 projects.
- Review and apply succeeded.
- After conservative obsolete-session reconciliation, exactly 8 desired
  sessions were active.
- Configured exclusions were absent in the guest.
- Repository instructions and SQL source were present.
- Recursive guest grep read project source and found 2,039 matching C# files.
- Host Git and `gh` proxying worked from a guest project.
- Host file descriptors were 20,191 of 491,520 before the run and 21,327 of
  491,520 after, about 4.34% of the limit, with no whole-tree mount
  exhaustion.
