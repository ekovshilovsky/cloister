<!-- Proprietary and confidential. All rights reserved. -->

# Workspace and VCS broker design

Status: implementation contract

Scope: opt-in multi-project workspaces for broker profiles, plus host-executed Git and GitHub CLI access from synchronized guest projects. Existing single-project broker and virtiofs profiles keep their current behavior.

## Decisions

1. `workspace.mode: workspace` selects a bounded collection of projects. `workspace.root` is a host routing root. `workspace.selectors` are relative glob patterns, defaulting to `apps/*` and `tools/*`. Only matching directories are sessions. A selector cannot escape the root, and duplicate or nested matches are rejected.
2. Cloister coordinates the collection through the existing `SyncBroker` session API instead of generating `mutagen.yml`. This preserves the tested isolated Mutagen daemon, SSH wrapper, status parser, policy hash, and rollback behavior. The collection has the same start, flush, pause, and terminate semantics as a Mutagen project: each action applies to every named session and fails closed if any session is unhealthy.
3. Every project is synchronized as a whole directory. Discovery does not use `git ls-files`, so tracked files, untracked files, and local directories participate unless an ignore rule excludes them.
4. Every session uses `two-way-safe`, portable symlinks, accelerated scanning, the host's native watcher, `probeMode: assume`, `maxEntryCount`, and `maxStagingFileSize`. The default entry limit is 200,000 per project. The initial full scan is bounded by the project root and the limit.
5. `.git` is always excluded. Git and `gh` invoked in a mapped guest project execute on the host through an authenticated loopback command service and an SSH reverse tunnel. Host Git continues to run normally against the same repository.

## Configuration

```yaml
profiles:
  work:
    start_dir: ~/Code/1-800-Battery
    workspace:
      mode: workspace
      root: ~/Code/1-800-Battery
      selectors:
        - apps/*
        - tools/*
      max_entry_count: 200000
      max_staging_file_size: 2 GiB
      project_ignore:
        tools/rockauto-scraper:
          - data/raw/
        apps/example:
          - .local-generated/
```

`root` defaults to `start_dir`. Empty selectors use `apps/*` and `tools/*`. `max_entry_count` and `max_staging_file_size` apply to each session. `project_ignore` keys are clean project paths relative to `root`, not patterns. Unknown keys are rejected so a typo cannot silently omit a safety rule.

Single-project `mode: broker` continues to use `start_dir` as its one session root and `workspace.ignore` as its extra rules. `mode: virtiofs` remains unchanged.

## Session model

For every discovered project, Cloister calls the existing stable session builder. The host endpoint is the canonical project directory. The guest endpoint is `~/workspaces/<sanitized-name>-<path-hash>`. The hash prevents collisions between projects with the same leaf name and remains stable across restarts. The session name contains the profile identifier and the same opaque project identifier.

The ordered ignore policy is intentionally small:

1. Profile `workspace.ignore` rules, parsed by the existing ignore compiler.
2. Rules from the exact `project_ignore` entry. Project-specific output directories (for example a scraper's `data/raw/`) are expressed here rather than hardcoded in Cloister.
3. Mandatory `.git` and `node_modules` exclusions, appended last so negation cannot re-include them.

Workspace mode does not import repository `.gitignore` files because those files commonly hide local build and runtime inputs that still need to exist in the guest. It also does not add generic build, cache, coverage, distribution, virtual environment, or generated-output exclusions. Such content remains available to build, test, deploy, commit, and run unless the profile explicitly excludes it. Ignored paths are not scanned, transferred, or deleted by Mutagen, so a guest-local `node_modules` survives synchronization.

Activation creates every safe guest root, creates or resumes every session, then flushes and verifies every session before entry. Partial activation is rolled back by pausing every session that was touched. Quiesce flushes and verifies each active session before pausing or terminating it. A session already paused with no reported conflicts or endpoint problems is already quiesced, so Cloister skips its invalid flush and redundant pause. A failure leaves the VM running and reports the project that failed.

The broad `start_dir` traversal refusal is disabled only for `mode: workspace`. The routing root is not mounted or synchronized. Each discovered project still has an independent `maxEntryCount` fail-fast limit. The existing refusal remains for single-project broker and virtiofs modes.

## VCS control and data flow

```text
guest git shim
  -> HTTP request on guest loopback with token, cwd, argv, and selected env
  -> SSH reverse tunnel
  -> host VCS command service
  -> validate and classify the command
  -> account-scoped gh: run from host home without a project barrier
  -> remote-only repo-scoped gh with explicit -R/--repo/GH_REPO: use a mapped project when available, otherwise run from host home
  -> other commands: map guest cwd, lock that project, and flush/verify Mutagen
  -> real host git or gh
  -> for working-tree-mutating commands: Mutagen flush and clean-status barrier
  -> streamed combined output and exact exit code trailer
```

The service listens on a random host loopback port. SSH exposes it on a guest loopback port for the lifetime of `cloister enter` or `cloister exec`. A random bearer token is written into a guest file readable only by the VM user. The endpoint accepts only `git` and `gh`, bounded request fields, and a valid token. Git and implicitly targeted repository commands also require a registered workspace path; account-scoped `gh` and explicitly targeted repository commands do not. It never accepts a shell command string. Arguments are transported as repeated URL-encoded values and passed directly to `exec.Cmd`.

The guest installs `git` and `gh` shims ahead of the system tools. Inside a registered workspace they call the broker. Outside a registered workspace they execute the saved real guest binary when present. A missing or empty legacy record falls back only to the base-owned `/usr/bin/<tool>`; if neither is available, the shim returns a clear unmapped-workspace error. This client shape and protocol can later add another named VCS executable without changing path mapping or synchronization barriers.

Commands tied to a mapped project flush before execution. A successful flush is a bidirectional convergence barrier, which makes guest edits visible in the host working tree and also imports completed host edits. Commands classified as working-tree-mutating flush again after the host process exits, even when it exits nonzero because a failed merge, rebase, checkout, or download can leave changes. The post-flush result takes precedence over a zero command exit because the guest view would otherwise be stale. Per-project locking serializes brokered VCS operations. Account-scoped `gh` commands and explicitly targeted remote-only repository commands issued outside a project have no project tree to synchronize, so they run from host home without a lock or barrier. Repository verbs that can consume, create, or modify local files still require a mapped project even when `-R`, `--repo`, or `GH_REPO` supplies the remote repository. Uncoordinated native host editors cannot be locked; concurrent edits are reconciled by the barriers and fail closed on a Mutagen conflict.

Mutating Git verbs are `checkout`, `switch`, `reset`, `merge`, `pull`, `stash`, `rebase`, `restore`, `clean`, `revert`, `cherry-pick`, `am`, `apply`, `submodule`, `commit`, and `push`. Commit and push receive the post-barrier because host hooks can modify tracked files. Other Git commands get the pre-flush only. This is conservative about guest refresh without making ordinary `status`, `diff`, `log`, `show`, `branch`, or `fetch` wait for an unnecessary second scan. A config-defined override is intentionally omitted until a real command requires one.

## Edge cases

The first protocol implements noninteractive host execution, path mapping, host credentials, host hooks, constrained `gh`, initialized submodules, and synchronization fences. Full PTY and stdin transport, terminal credential prompts, arbitrary interactive editors, TTY-dependent hooks, and interactive conflict tools are explicitly deferred.

| Case | Behavior |
|---|---|
| `git status`, `diff`, `log`, `show`, `branch` | Flush and verify first, execute host Git at the mapped host cwd, stream output, return its exit code. |
| Checkout, reset, merge, pull, stash, rebase, restore | Flush and verify, execute under the project lock, then flush and verify again even after nonzero Git exit. |
| Commit message with `-m`, `-F`, or stdin-free hook | Supported. Git and hooks run on the host with the host repository and configuration. |
| Interactive commit editor | No terminal byte stream is carried by the first protocol version. A commit without `-m`, `-F`, `--message`, or `--file` fails before execution with guidance to supply a message. Only the noninteractive `GIT_EDITOR=true` and `GIT_EDITOR=:` sentinels are forwarded. Arbitrary editors and PTY passthrough are deferred. |
| Other interactive Git modes | The request has no stdin or PTY. Commands such as `git add -p`, `git rebase -i`, and conflict tools fail early with guidance to run the equivalent host command. Noninteractive flags remain supported. |
| Push credentials | Git runs on the host and therefore uses host credential helpers, SSH agent, keychain, and configuration. A credential flow that requires terminal input is unsupported and fails rather than reading guest secrets. |
| Git hooks and signing | Hooks execute host-side in the host repository. Host signing and hook dependencies are used. Hook stdout and stderr are streamed. Hooks requiring a TTY are unsupported. |
| `gh` and PRs | The `gh` shim runs allowed host commands with host authentication. Search, status, `auth status`, `repo list`, and read-only API endpoints without repository placeholders or a `repos/...` path are account-scoped and need no mapped project. Remote-only PR, issue, run, workflow, release, `repo view`, and repository API operations may use `-R`/`--repo` or `GH_REPO` outside a project. Verbs that can use local files—including checkouts, downloads, uploads, file-backed bodies or notes, release assets, and workflow `@file` inputs—always require a mapped project. `gh api` remains restricted to read-only (GET, no request-body fields) because it runs with host credentials and an unrestricted write would allow account-level mutation (SSH keys, repo deletion) from the sandbox; run write API calls on the host. Aliases, extensions, authentication mutation, and repo clone or creation are rejected. Commands that invoke an editor or browser require explicit noninteractive flags. |
| Outside a mapped project | At the workspace routing root, account-scoped `gh` and explicitly targeted remote-only repository commands run from host home. Repository commands that can use local files are refused with guidance to change directory. Bare Git is refused with guidance to use `git -C`; implicitly targeted repository `gh` commands are refused with guidance to use `-R` or change directory. Outside `~/workspaces`, the shim still falls through to the real guest executable when available. No project is guessed. |
| Nested host repository | Mapping preserves the cwd relative path. Host Git discovers the nearest host-side `.git`, so an existing nested repository works. A nested repository created only in the guest cannot work because its `.git` is excluded; initialize it on the host first. |
| Submodule | An already initialized host submodule works because the host has its `.git` file and metadata while its working tree is synchronized. `git submodule update` executes host-side and the post-flush sends changes to the guest. Missing host submodule metadata produces the native Git error. `git submodule foreach` is rejected because it would accept an arbitrary host shell command. |
| Git worktree | A selected worktree is a distinct canonical session. Its host-side `.git` indirection remains host-only and Git resolves it normally. |
| Concurrent host edit | The pre-barrier imports edits completed before Git starts. The project lock blocks other brokered commands. An external edit during Git is not hidden; the post-barrier reconciles it or reports a conflict and fails closed. |
| Host process or tunnel loss | The shim reports broker unavailability and does not invoke guest Git against a metadata-free tree. Git's exit code is preserved when the command ran. |
| Flush or status failure | Git is not run after a failed pre-barrier. A failed post-barrier is reported as synchronization failure, and the original Git exit is included in diagnostics. |
| Unknown command classification | Unknown Git and `gh` subcommands are rejected before host execution. New allowlisted commands that mutate the working tree must be added to the explicit classifier with a test. |

## Failure handling and recovery

- Discovery fails on an invalid selector, an escaping match, a non-directory match requested as a project, nested selected roots, duplicate canonical roots, no projects, or an unused `project_ignore` key.
- A session hitting `maxEntryCount` or `maxStagingFileSize` fails activation with its relative project name. Cloister does not widen the limit automatically.
- A changed or unverifiable ignore policy is never resumed. Cloister logs the recovery, terminates the stale deterministic session, and creates a fresh synchronization history. If termination fails, recreation is refused and the existing session is left for inspection.
- Any workspace activation or quiesce error is annotated with the project path. Destructive VM operations do not continue after an incomplete flush.
- The VCS service rejects traversal, symlink escape, an unknown guest root, an unknown executable, invalid token, oversized request, and NUL-containing fields.
- The shim never falls back to host execution for an unmapped path. It also never falls back to guest Git inside a mapped project because the guest tree has no `.git` contract.

## Verification boundary

Unit tests use temporary project trees, `broker.Mock`, `vm.MockBackend`, and a fake host command runner. They cover discovery, stable session construction, minimal ignore composition, special and per-project ignores, per-session limits, guest-to-host path containment, Git command classification, and flush-run-flush ordering.

Real VM and Mutagen end-to-end verification remains necessary for Mutagen's `probe-mode=assume` CLI behavior, initial multi-session scans, reverse tunnel lifetime, HTTP streaming and trailers through both backends, guest PATH ordering, host credential helpers, hooks, submodules, and conflict behavior under concurrent host edits.

## Revisit later

If workspace collections grow into hundreds of projects, generate a private `mutagen.yml` and use `mutagen project` lifecycle calls to reduce subprocess overhead. The engine-neutral session collection is kept deliberately compatible with that replacement. A framed full-duplex transport with PTY allocation can later add interactive editors and prompts without changing host path mapping or Git execution semantics.
