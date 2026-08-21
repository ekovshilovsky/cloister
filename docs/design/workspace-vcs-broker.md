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

1. Nested repository `.gitignore` rules compiled by the existing ignore compiler.
2. Profile `workspace.ignore` rules.
3. Rules from the exact `project_ignore` entry.
4. The special `data/raw/` rule for `tools/rockauto-scraper`.
5. Mandatory `.git` and `node_modules` exclusions, appended last so negation cannot re-include them.

Workspace mode does not add generic build, cache, coverage, distribution, virtual environment, or generated-output exclusions. Such content remains available to build, test, deploy, commit, and run unless the repository or profile explicitly excludes it. Ignored paths are not scanned, transferred, or deleted by Mutagen, so a guest-local `node_modules` survives synchronization.

Activation creates every safe guest root, creates or resumes every session, then flushes and verifies every session before entry. Partial activation is rolled back by pausing every session that was touched. Quiesce flushes and verifies the complete set before pausing or terminating it. A failure leaves the VM running and reports the project that failed.

The broad `start_dir` traversal refusal is disabled only for `mode: workspace`. The routing root is not mounted or synchronized. Each discovered project still has an independent `maxEntryCount` fail-fast limit. The existing refusal remains for single-project broker and virtiofs modes.

## VCS control and data flow

```text
guest git shim
  -> HTTP request on guest loopback with token, cwd, argv, and selected env
  -> SSH reverse tunnel
  -> host VCS command service
  -> map guest cwd to one registered SessionSpec
  -> lock that project
  -> Mutagen flush and clean-status barrier
  -> real host git or gh, cwd mapped into the host project
  -> for working-tree-mutating commands: Mutagen flush and clean-status barrier
  -> streamed combined output and exact exit code trailer
```

The service listens on a random host loopback port. SSH exposes it on a guest loopback port for the lifetime of `cloister enter` or `cloister exec`. A random bearer token is written into a guest file readable only by the VM user. The endpoint accepts only `git` and `gh`, a registered workspace path, bounded request fields, and a valid token. It never accepts a shell command string. Arguments are transported as repeated URL-encoded values and passed directly to `exec.Cmd`.

The guest installs `git` and `gh` shims ahead of the system tools. Inside a registered workspace they call the broker. Outside a registered workspace they execute the saved real guest binary when present, otherwise return a clear unmapped-workspace error. This client shape and protocol can later add another named VCS executable without changing path mapping or synchronization barriers.

All commands flush before execution. A successful flush is a bidirectional convergence barrier, which makes guest edits visible in the host working tree and also imports completed host edits. Commands classified as working-tree-mutating flush again after the host process exits, even when it exits nonzero because a failed merge, rebase, or checkout can leave changes. The post-flush result takes precedence over a zero command exit because the guest view would otherwise be stale. Per-project locking serializes brokered VCS operations. Uncoordinated native host editors cannot be locked; concurrent edits are reconciled by the barriers and fail closed on a Mutagen conflict.

Mutating Git verbs are `checkout`, `switch`, `reset`, `merge`, `pull`, `stash`, `rebase`, `restore`, `clean`, `revert`, `cherry-pick`, `am`, `apply`, `submodule`, and `worktree`. Other Git commands get the pre-flush only. This is conservative about guest refresh without making ordinary `status`, `diff`, `log`, `show`, `branch`, `fetch`, `push`, or commit wait for an unnecessary second scan. A config-defined override is intentionally omitted until a real command requires one.

## Edge cases

| Case | Behavior |
|---|---|
| `git status`, `diff`, `log`, `show`, `branch` | Flush and verify first, execute host Git at the mapped host cwd, stream output, return its exit code. |
| Checkout, reset, merge, pull, stash, rebase, restore | Flush and verify, execute under the project lock, then flush and verify again even after nonzero Git exit. |
| Commit message with `-m`, `-F`, or stdin-free hook | Supported. Git and hooks run on the host with the host repository and configuration. |
| Interactive commit editor | No terminal byte stream is carried by the first protocol version. A commit without `-m`, `-F`, `--message`, or `--file` fails before execution with guidance to supply a message. `GIT_EDITOR=true` remains usable for workflows that intentionally reuse a prepared message. |
| Other interactive Git modes | The request has no stdin or PTY. Commands such as `git add -p`, `git rebase -i`, and conflict tools fail early with guidance to run the equivalent host command. Noninteractive flags remain supported. |
| Push credentials | Git runs on the host and therefore uses host credential helpers, SSH agent, keychain, and configuration. A credential flow that requires terminal input is unsupported and fails rather than reading guest secrets. |
| Git hooks and signing | Hooks execute host-side in the host repository. Host signing and hook dependencies are used. Hook stdout and stderr are streamed. Hooks requiring a TTY are unsupported. |
| `gh` and PRs | The `gh` shim uses the same mapping and pre-flush barrier, then runs host `gh` with host authentication. Commands that invoke an editor or browser may require explicit noninteractive flags. |
| Outside a mapped project | Fall through to the real guest executable. If it is absent, return an error naming the unmapped cwd. No host path is guessed. |
| Nested host repository | Mapping preserves the cwd relative path. Host Git discovers the nearest host-side `.git`, so an existing nested repository works. A nested repository created only in the guest cannot work because its `.git` is excluded; initialize it on the host first. |
| Submodule | An already initialized host submodule works because the host has its `.git` file and metadata while its working tree is synchronized. `git submodule update` executes host-side and the post-flush sends changes to the guest. Missing host submodule metadata produces the native Git error. |
| Git worktree | A selected worktree is a distinct canonical session. Its host-side `.git` indirection remains host-only and Git resolves it normally. |
| Concurrent host edit | The pre-barrier imports edits completed before Git starts. The project lock blocks other brokered commands. An external edit during Git is not hidden; the post-barrier reconciles it or reports a conflict and fails closed. |
| Host process or tunnel loss | The shim reports broker unavailability and does not invoke guest Git against a metadata-free tree. Git's exit code is preserved when the command ran. |
| Flush or status failure | Git is not run after a failed pre-barrier. A failed post-barrier is reported as synchronization failure, and the original Git exit is included in diagnostics. |
| Unknown command classification | It receives the safe pre-flush behavior. New commands that mutate the working tree must be added to the explicit classifier with a test. |

## Failure handling and recovery

- Discovery fails on an invalid selector, an escaping match, a non-directory match requested as a project, nested selected roots, duplicate canonical roots, no projects, or an unused `project_ignore` key.
- A session hitting `maxEntryCount` or `maxStagingFileSize` fails activation with its relative project name. Cloister does not widen the limit automatically.
- A changed ignore policy keeps the existing fail-closed behavior: the session cannot resume with stale exposure rules and must be terminated and recreated deliberately.
- Any workspace activation or quiesce error is annotated with the project path. Destructive VM operations do not continue after an incomplete flush.
- The VCS service rejects traversal, symlink escape, an unknown guest root, an unknown executable, invalid token, oversized request, and NUL-containing fields.
- The shim never falls back to host execution for an unmapped path. It also never falls back to guest Git inside a mapped project because the guest tree has no `.git` contract.

## Verification boundary

Unit tests use temporary project trees, `broker.Mock`, `vm.MockBackend`, and a fake host command runner. They cover discovery, stable session construction, minimal ignore composition, special and per-project ignores, per-session limits, guest-to-host path containment, Git command classification, and flush-run-flush ordering.

Real VM and Mutagen end-to-end verification remains necessary for Mutagen's `probe-mode=assume` CLI behavior, initial multi-session scans, reverse tunnel lifetime, HTTP streaming and trailers through both backends, guest PATH ordering, host credential helpers, hooks, submodules, and conflict behavior under concurrent host edits.

## Revisit later

If workspace collections grow into hundreds of projects, generate a private `mutagen.yml` and use `mutagen project` lifecycle calls to reduce subprocess overhead. The engine-neutral session collection is kept deliberately compatible with that replacement. A framed full-duplex transport with PTY allocation can later add interactive editors and prompts without changing host path mapping or Git execution semantics.
