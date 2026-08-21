# Filesystem broker verdict

Status: neutral architecture verdict

Date: 2026-08-20

Scope: choose the first implementation path for a project-scoped, fd-safe Cloister filesystem boundary used by AI coding agents in a Linux VM.

## Decision

Implement Tier 3 Phase 0 first, followed only by a read-only vertical slice if the platform gates pass. Do not treat that choice as approval to ship a new writable filesystem. Keep Tier 2 as the immediate fallback implementation and as an explicit reduced-fidelity mode.

This is a narrow win for Tier 3, not an endorsement of its schedule or its first-pass confidence. Tier 2 is more likely to preserve bytes correctly in its first writable prototype because it delegates reconciliation and atomic destination replacement to a mature engine. It nevertheless removes normal guest Git behavior by design and cannot give a host save followed immediately by a guest command a synchronous visibility guarantee. For Claude, Cursor, and Codex working inside the VM, the missing repository database is workflow-disqualifying, not merely eventual consistency. That architectural mismatch justifies spending the first few days on Tier 3's feasibility gates before committing to Tier 2's knowingly reduced product.

No Tier 3 writable mode should become the default during the first two weeks. Phase 0 can reject the architecture, but it cannot prove writable filesystem correctness. A differential test matrix, fault injection, real editors, real watchers, and staged rollout can contain the risk only if every unsupported operation fails explicitly and every phase remains kill-gated.

## Repository sanity check

The shared integration analysis in both blueprints is substantially correct:

- `internal/vm/mount.go` always prepends the resolved workspace as the first writable mount. `MountsChanged` compares only slice lengths.
- `internal/vm/backend.go` passes workspace and supplemental mounts through one positional `Start` method. There is no attachment lifecycle separate from VM startup.
- `cmd/enter.go` resolves `StartDir`, calls `BuildMounts`, and starts the VM directly. The same construction appears across create, rebuild, reset, resize, snapshot, and add-stack flows, with start recovery calling the backend again.
- `internal/config/defaults.go` defaults `StartDir` to `~/code`, which is a routing hint or broad parent, not a safe project boundary.
- `internal/vm/lume/backend.go` silently passes only `mounts[0]`. Removing the first workspace mount would make the first supplemental mount accidentally privileged unless this behavior is fixed or rejected.
- Colima explicitly selects `virtiofs` for every persistent mount and passes `mount_inotify`. Either tier therefore needs the same initial mount separation and lifecycle centralization work.

These seams make the common integration work credible. They do not reduce the difficulty of Tier 3's filesystem semantics.

## Scoring method

The winner in the table is the design with the better expected correctness if implemented next by an AI-assisted team, not the design with the higher theoretical ceiling. Trust measures confidence in the winning design's stated handling:

- High: behavior is delegated to a mature native filesystem or mature synchronization mechanism, with a narrow testable contract.
- Medium: the design is plausible and testable, but important races or platform differences remain.
- Low: correctness depends on new distributed filesystem, kernel, metadata, or recovery behavior that examples and Phase 0 cannot establish.

Where Tier 3 has the better contract but not the safer first implementation, the distinction is stated explicitly.

## Independent correctness scorecard

| Surface | More correct first implementation | Trust | Independent judgment |
|---|---|---:|---|
| Read-after-write | Tier 3 | Medium | A guest write can complete only after the host syscall, and a new guest open can read host state directly. Tier 2 has no cross-endpoint read-after-write guarantee. Its `flush` is an explicit barrier, so `host save; guest build` can race. Tier 3 still has event latency and FUSE cache invalidation risk for host-originated writes and already-open handles, so “strict” must not be described as globally instantaneous. |
| Editor atomic save | Tier 2 | High | Each endpoint uses a native filesystem. Mutagen stages a complete receive-side file and atomically places the final version, which is a strong no-torn-file property even though it does not reproduce the original temp-file and rename event stream. Tier 3's one-call host rename is the better eventual contract, but correct open-target identity, metadata ordering, invalidation, replay, and error reporting make the first writable pass low confidence. Tier 3 overstates how mechanical this is. |
| Rename, including exchange | Tier 3 contract, Tier 2 first-pass safety | Low for Tier 3 fidelity | Darwin exposes atomic rename-over, exclusive rename, and swap primitives, so Tier 3 can eventually preserve the operation. The hard work is the broker object graph, directory descendants, caches, watcher cookies, metadata WAL, and replay. Tier 2 safely converges final content but may model rename as delete plus create, loses inode identity, and has no cross-endpoint exchange contract. Tier 3 wins only after differential and crash gates. |
| Unlink while open | Tier 2 for practical safety | High within each endpoint | Native APFS and the guest filesystem already preserve local open-unlink behavior. Tier 2 does not preserve one shared inode identity across endpoints, but it also does not invent one. Tier 3's hardlink pin is plausible, yet it changes host link count and ctime, creates FSEvents churn, cannot pin directories, and depends on exact journal and cleanup behavior. Tier 3's stated handling is low to medium confidence until stress-tested. |
| `mmap` | Tier 2 | Medium | Guest and host mappings are locally native, with divergence handled as synchronization or conflict rather than shared coherent memory. That is not shared-filesystem fidelity, but it is understandable and unlikely to corrupt a local mapping. Tier 3 depends on a recent FUSE protocol capability, dirty-page ordering, invalidation, `msync`, truncate races, and error propagation. The direct-I/O claim is a platform gate, not an implementation plan. Tier 3 materially overstates first-pass feasibility here. |
| `fsync` durability | Tier 2 for initial data safety, Tier 3 only after proof | High for Tier 2's documented boundary | In Tier 2, `fsync` durably protects the guest copy and an explicit successful broker flush protects convergence to the host. It does not make guest `fsync` a host durability barrier, which must be visible in the product contract. Tier 3 promises the stronger boundary, but host data, guest-only metadata, pins, namespace changes, and a WAL cannot be assumed crash-consistent merely because each has an `fsync`. Trust Tier 3 only after acknowledgement-ledger fault tests. |
| Symlinks | Tier 2 | High for safe portable links | Mutagen portable mode narrows semantics but rejects escaping links and uses fixed endpoints. Tier 3 can preserve raw link text while refusing escape on dereference, which is more faithful, but every path-taking opcode and every rename race must use the correct beneath-root primitive. One missed path is a security failure. Tier 3's first-pass path-escape confidence is overstated even though the required Darwin primitives exist on the current target SDK. |
| Permissions, UID/GID, xattrs, hardlinks | Tier 2 when it fails closed | Medium | Tier 2 explicitly preserves only the executable distinction and mapped ownership. It should preflight and reject projects requiring hardlink identity or material metadata. That is limited but honest. Tier 3's metadata database, ACL enforcement, link-count masking, setid rules, timestamps, and host upper-bound permissions form another filesystem inside the filesystem. Its contract is better, but first-pass correctness is low. Tier 2 understates how often mode, timestamp, Git, packaging, and generated-code workflows notice these losses. |
| Inotify and watchers | Tier 2 for mainstream final-state workflows | Medium | Mutagen applies real operations to a native guest filesystem, so guest watchers receive valid events for the staged apply sequence and read the updated guest state. The events are not the host's original syscall stream. Tier 3 correctly notes that stock FUSE invalidation cannot synthesize arbitrary fsnotify events, but its kernel extension still receives information derived from coalescing and lossy FSEvents. It cannot reconstruct an exact stream that the host never supplied. Tier 3 overstates broad watcher fidelity. Application-level rebuild and overflow behavior, not event-for-event identity, must be the gate. |
| `.git` and guest VCS | Tier 3 | Medium only with host-backed `.git`, Low for the proposed bridge | Tier 2 intentionally removes the repository database, index, hooks, refs, worktree metadata, and normal Git commands. That breaks core agent context, safety checks, diff review, commits, and many build/version scripts. Tier 3 can expose `.git` through the bounded broker and preserve the current shared-worktree model. Its guest-local `.git` plus bundle bridge is a separate, high-risk VCS product and should not be on the critical path. Tier 2 most seriously understates workflow breakage here. |
| Crash and detach | Tier 2 | High | Mutagen's existing three-way history, atomic staging, persistent guest disk, conflict state, pause, resume, and explicit flush give a much smaller new recovery surface. Tier 3 must recover request deduplication, uncertain namespace outcomes, pins, mappings, guest metadata, event generations, and disconnects without retargeting handles. A WAL is necessary, not sufficient. Tier 3's first-pass and two-to-five-day confidence is substantially overstated. |
| Path-escape security | Tier 2 | High | A fixed Mutagen endpoint plus portable symlinks and canonical activation is the narrower attack surface. Tier 3 has a sound fail-closed design based on root-relative Darwin operations, but it exposes a large opcode parser and path resolver to a compromised guest. Its claim is credible only after syscall-level race auditing and fuzzing. No string-based containment fallback is acceptable. |

### Scorecard interpretation

Tier 2 wins most first-pass safety categories because applications operate on native filesystems and an established sync engine moves complete states. Tier 3 wins the two categories most central to an in-VM coding agent, synchronous live-tree behavior and guest VCS, and has the higher ceiling for rename and metadata fidelity. This is why the decision is not a simple count of rows.

Tier 2's limitations are mostly explicit, which reduces silent corruption risk. The dangerous exception is presenting the result as a transparent workspace replacement. A stale editor buffer can overwrite a previously synchronized host change as an apparently intentional one-sided guest edit. `two-way-safe` cannot distinguish that serial stale save from a deliberate revert. A command run immediately after a host save can also consume the old guest copy without any conflict. These are normal consequences of replication, but they are silent if the UI and launch flow imply a live filesystem.

Tier 3's specification is unusually honest about many hard cases, but the estimates remain optimistic. In particular, these are not mechanical first-pass tasks:

- Preserving open-handle identity with hardlink pins without causing ctime, link-count, backup, indexer, or FSEvents feedback problems.
- Combining visible host namespace operations with guest-only metadata and a recoverable WAL across every crash point.
- Implementing `MAP_SHARED`, truncate, dirty-page invalidation, `msync`, and host-writer races on a controlled but evolving guest kernel.
- Producing watcher behavior from FSEvents that is sufficient for real tools. A kernel injection ABI cannot recreate information already coalesced or dropped by macOS.
- Enforcing beneath-root resolution for every operation, including dual-path rename, links, xattrs, and recovery, under concurrent host renames.
- Maintaining inode generation, directory-handle, link-count, lock-owner, and replay semantics across broker and VM restarts.
- Building a safe `.git` semantic bridge. That is not a small overlay feature.

## The crux for AI coding agents

### Are Tier 2's limits acceptable?

Stale-read is acceptable only with an explicit workflow boundary. It is workable when all edits and builds happen on the guest, or when host-to-guest commands are wrapped in a successful `flush` barrier. It is not acceptable for an interface that invites a host editor save followed immediately by an independent guest build and claims local-filesystem behavior.

Metadata loss is project-dependent. For ordinary source trees, executable-bit preservation plus preflight rejection of hardlinks and material xattrs may be acceptable. It is not safe to silently normalize arbitrary modes, ownership, timestamps, or link identity. Build systems that derive freshness from mtimes, packaging repositories, filesystem test suites, and repositories with linked fixtures must be rejected or explicitly opted into a reduced contract.

The mandatory `.git` exclusion is workflow-disqualifying for the stated default workload. Claude, Cursor, and Codex routinely use `git status`, `git diff`, repository roots, ignore rules, history, and worktree state to understand and validate changes. A warning that Git remains host-side does not restore those capabilities. It changes the product from “coding inside an isolated VM” to “editing a synchronized copy with external VCS control.” That can be a useful fallback mode, but it is not a transparent replacement for the current workspace.

Tier 2 therefore understates workflow breakage when it calls `.git` merely the largest tradeoff. It is a release-defining product boundary. It also understates the effect of mandatory global exclusions such as every directory named `build` or `target`, the need for users and agents to remember flush barriers, and the stale-buffer overwrite that safe three-way reconciliation cannot identify.

### Is Tier 3's corruption risk containable?

It is containable as a research and staged implementation program, not by Phase 0 alone and not on the proposed schedule. Phase 0 can validate transport ownership, kernel capabilities, Darwin primitives, and whether event injection is possible. It cannot validate mutation atomicity, durability, mmap ordering, cache coherence, or recovery.

The risk becomes containable only if development follows three rules:

1. Capability honesty: unsupported behavior returns an accurate error. No opcode may return success as a placeholder, and no degraded watch or cache mode may be labeled full correctness.
2. Differential release gates: compare namespace, bytes, metadata, return codes, open-handle identity, and application-observed events against a native reference across randomized traces and real tools.
3. Acknowledgement-based fault testing: after every injected crash, each successful mutation or durability acknowledgement must be present or recoverable, each failed or uncertain operation must remain explicit, and no handle may retarget.

Even with those rules, test coverage is evidence, not a proof of complete POSIX equivalence. Tier 3 should remain read-only, then opt-in writable for disposable Git-clean projects, until sustained use shows no silent mismatch.

## First two weeks of AI-assisted work

The goal of the first two weeks is a defensible go or no-go decision and a tested read-only slice, not general availability.

### Days 1 and 2: establish the oracle and common seam

1. Record the current worktree status and preserve unrelated files. Add no broad filesystem changes outside the feature branch.
2. Build a trace corpus from the actual target tools: Cursor or VS Code atomic save, Vim save, Git status and commit, Node and Go watchers, compiler reads, rename-over-open, unlink-open, shared mmap, and fsync.
3. Define observable contracts for fresh open, existing open handle, final namespace, durability acknowledgement, watcher overflow, and explicit unsupported errors.
4. Implement or prepare the common `StartOptions` or `StartSpec` seam, `BuildSupplementalMounts`, deep mount comparison, and one lifecycle coordinator behind tests. A static check must find no command-layer production call that can reconstruct a workspace mount.
5. Make Lume reject unsupported broker or supplemental-mount combinations. Do not let its current first-mount behavior silently choose a different share.

### Days 3 through 5: Tier 3 Phase 0, with realistic scope

1. Prove an authenticated request and reconnect path from a Colima Linux guest through the actual relay ownership chain to a host Unix socket.
2. Probe the exact supported macOS volume for beneath-root open, dual-path rename, swap, exclusive rename, hardlinks, directory sync behavior, xattrs, and full sync. Record capability failures rather than inferring support from APFS alone.
3. Audit the low-level FUSE library and target guest kernel for protocol 7.37 `TMPFILE`, 7.39 direct-I/O shared mmap, rename flags, syncfs, invalidation, and restart behavior.
4. Prototype one host create, modify, rename pair, delete, and overflow through the guest-kernel event extension, then run one real Node watcher and one real editor watcher.
5. Prototype hardlink pin creation and removal under a read/open/unlink/replace loop while measuring source ctime, FSEvents feedback, pin leakage, and host fd counts.

Hold a formal gate at the end of day 5. Two to four elapsed hours is not credible for all of these platform questions. Two to three focused days is aggressive but useful.

### Days 6 through 9, only if Phase 0 passes: read-only vertical slice

1. Implement the bounded protocol, capability handshake, attachment authorization, project identity, contained lookup, getattr, readlink, open record, read, readdirplus, release, and clean detach.
2. Mount one project with no workspace virtiofs entry. Keep `.git` visible in this experiment so ordinary Git reads exercise the intended workflow. Do not implement the `.git` bundle bridge yet.
3. Differentially compare path lookup, errors, content, inode stability while referenced, symlinks, directory iteration, Unicode, case behavior, root replacement, reconnect, and a million-entry traversal.
4. Run Claude, Cursor, and Codex read-only repository discovery plus Git status and diff. Record latency and all missing operations.

### Days 10 through 14, only if the read-only slice passes: narrow mutation core

1. Implement regular-file create, write, truncate, named-temp atomic rename-over, unlink-open pins, fsync, fsyncdir, syncfs, and the minimum WAL needed for those operations.
2. Keep exchange, `O_TMPFILE`, writable shared mmap, ACLs, guest-only ownership, complex xattrs, locks, and the `.git` bridge disabled unless their individual gates pass. Return `EOPNOTSUPP` or another exact error, never success.
3. Add fault injection before and after every host syscall, WAL intent, WAL commit, response send, reconnect, and final pin release.
4. Run real editor saves and a randomized model trace on disposable, backed-up fixtures. Do not point the writable slice at the user's only copy of a project.
5. At day 14, either continue Tier 3 as an opt-in research track, or stop it and implement Tier 2 behind the same lifecycle and broker interface.

If more AI capacity is available, Tier 2's Mutagen executable spike can run independently during days 3 through 5. It must not delay or soften the Tier 3 gate, and it must remain read-only with respect to real projects until its own conflict and crash tests pass.

## Exact kill gates

Failure of any gate marked “flip” stops the Tier 3 writable path and makes Tier 2 the first shippable implementation. A failed gate is not converted into a warning or relaxed default.

### Phase 0 flip gates, due by day 5

1. Transport and identity, flip: sustain one hour under load and 100 forced reconnects with zero duplicated committed mutation IDs, zero cross-profile attachment, and 100 percent rejection of expired, replayed, wrong-profile, and wrong-epoch tokens. If the actual Colima relay cannot be owned and restarted without patching an unstable private hypervisor interface, flip.
2. Containment primitives, flip: the supported macOS and target volume must provide root-relative, no-follow operations for all required single-path and dual-path mutations, or an audited fd-walk alternative must already exist. Run at least 1,000,000 concurrent traversal, symlink-swap, parent-rename, and root-replacement attempts with zero syscalls reaching an outside sentinel. Any escape or ambiguous root identity flips immediately.
3. FUSE capability, flip for the full design: the target guest kernel and chosen library must negotiate and expose the needed opcodes and invalidations without silently degrading them. Missing `TMPFILE` may be an explicit scoped limitation only if every target editor and tool passes with `EOPNOTSUPP`. Missing both direct-I/O shared mmap and a differential-tested cached fallback flips.
4. Watch feasibility, flip: for 100 repetitions of host create, atomic replace, rename, delete, subtree move, and a 10,000-event burst, each representative Node watcher, Go watcher, and editor must either observe a usable event and the correct final state, or receive overflow and demonstrably rescan. One silent missed final-state rebuild after the broker reports healthy flips. Exact host syscall reconstruction is not required because FSEvents cannot promise it.
5. Pin feasibility, flip: 100,000 open, rename, replace, unlink, reconnect, and close cycles must produce zero handle retargets, zero premature deletion, and zero leaked pins after recovery. Persistent self-generated FSEvents feedback, unbounded ctime churn that breaks target tools, or inability to create safe same-volume state on the supported project volume flips.

### Read-only gates, due by day 9

6. Descriptor bound, flip: after warmup, increasing a fixture from 20,000 to 1,000,000 visible files, 200,000 guest handles, and recursive guest watches may add no more than 32 persistent host descriptors. Peak broker descriptors for one attachment with 64 workers must remain below 512 plus a separately measured fixed launchd and FSEvents baseline, and must return to within 16 descriptors of baseline after detach.
7. Read and identity differential, flip: one million randomized supported read-only operations must have zero namespace, content, containment, or error-code mismatches outside explicitly documented network-filesystem cases. An open handle must never retarget after host rename, replace, or delete. Any silent mismatch flips.
8. VCS read workflow, flip for the target product: unmodified guest `git status`, `git diff`, repository-root discovery, ignore evaluation, and history reads must work on the representative project without exposing any host path outside the attachment. If neither safe host-backed `.git` nor another already-proven VCS mode can do this, Tier 3 has not justified itself over the Tier 2 fallback.

### Writable gates, evaluated from day 10 onward

9. Atomic-save and rename gate, flip: 10,000 saves for each target editor pattern, including a crash at every broker transition, must yield exactly the old complete file or the new complete file. There may be no missing destination, partial contents, unintended mode change, open-target retarget, or success reply for an uncommitted final namespace.
10. WAL and crash gate, flip: for every implemented mutation, inject broker, relay, guest-daemon, and VM failure before and after each transition. Across at least 100 repetitions per transition, every acknowledged operation must be present or deterministically completed during recovery, every unacknowledged uncertain result must be reported, no committed operation may be applied twice, and no handle may retarget. Journal corruption must fail read-only or `EIO`, never guess.
11. Durability gate, flip: after a successful `fsync`, `fdatasync`, or `syncfs`, forced broker and VM termination must retain all covered bytes and required metadata in 1,000 fault runs. Injected host sync errors must reach the guest and must prevent clean detach. If the test cannot distinguish an acknowledged durable boundary from buffered success, Tier 3 remains non-writable.
12. Mapping gate, flip for general writable use: 10,000 randomized `MAP_PRIVATE` and `MAP_SHARED` traces against a native reference must show no lost clean-host update, no discarded dirty guest page, correct truncate behavior, and correct `msync` and unmap error propagation. Documented concurrent writes to the same byte range may remain a data race, but no non-racing trace may differ silently.
13. Metadata gate, scope or flip: every advertised mode, owner mapping, timestamp, xattr, ACL, and hardlink operation must pass create, replace, rename, crash, and reconnect tests. A feature that cannot pass must be omitted from negotiation and return an exact error. If a representative project requires the missing feature, flip for that project class.
14. Git mutation gate, flip for agent default: guest `git add`, commit, checkout, reset, worktree discovery, hooks under the declared security policy, and crash recovery must preserve all new objects and refs. No automation may silently move the host branch or overwrite its index. One unrecoverable acknowledged commit blocks default rollout.

## Tier 2 fallback requirements

If a flip gate fails, reuse the common mount and lifecycle work and implement Tier 2. Ship it only with an explicit “synchronized copy” label, not as local-filesystem equivalence. Its minimum gates are:

- No project path appears in a production `--mount` or `--shared-dir` argument.
- Host descriptors stay bounded and do not grow with guest traversal.
- A successful attach and every host-to-guest command barrier wait for a clean Mutagen cycle.
- A normal detach, stop, rebuild, reset, or snapshot cannot report success with a failed flush or unresolved conflict.
- Engine or VM crashes never expose a partially transferred destination file.
- Conflict tests retain both divergent versions. Serial stale-buffer overwrites remain a documented limitation.
- Projects with included hardlinks, material xattrs, unsupported symlinks, or required metadata fail preflight unless the user explicitly accepts normalization.
- `.git` absence is shown before launching an agent. Tier 2 is not the default agent mode until product tests establish an acceptable host-side VCS workflow or a separately reviewed VCS capability exists.
- Mutagen version, machine interface, installation, provenance, and distribution license are pinned and approved before packaging.

## Bottom line

Tier 3 Phase 0 is the winner to implement first, because Tier 2's mandatory `.git` exclusion removes the repository context and VCS operations that make an isolated AI coding environment useful. This does not validate Tier 3's optimistic schedule: if its transport, containment, watcher, handle-identity, differential, or crash gates fail, stop the bespoke writable filesystem and ship Tier 2 only as an explicitly reduced-fidelity synchronized-copy fallback.
