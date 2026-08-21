# Filesystem broker verdict (independent cross-family review)

Status: independent judge opinion

Date: 2026-08-20

Author: independent cross-family judge (Claude), reviewing two designs and one prior verdict authored by a different model family (Codex).

Scope: decide which filesystem broker to implement first for a project-scoped, fd-safe Cloister boundary used by AI coding agents editing a project inside an isolated Linux VM. Judged primarily on correctness (will it silently break editors or toolchains, or lose data), with AI-assisted implementation speed as a secondary factor.

License: proprietary.

## How I formed this opinion

I read `docs/design/fs-broker.md` (Tier 2, Mutagen sync broker) and `docs/design/fs-broker-tier3.md` (Tier 3, guest-FUSE-over-vsock broker), then read the real integration points (`internal/vm/mount.go`, `internal/vm/backend.go`, `internal/vm/colima/backend.go`, `internal/vm/lume/backend.go`, `internal/config/defaults.go`, `cmd/enter.go`) to ground the judgment in the codebase. I built my own scorecard and verdict before opening `docs/design/fs-broker-verdict.md`. Only then did I compare.

Two structural facts from the code shape everything below:

1. The current `BuildMounts` unconditionally prepends the whole workspace as a writable virtiofs mount, and Colima uses virtiofs for every mount. That is the fd-per-inode exposure that caused ENFILE. The existing `--mount-inotify=false` comment in `internal/vm/colima/backend.go` already documents an fd leak ("one open directory handle per watched subdirectory and never reclaims aggressively ... leaks thousands of fds per hour"). The fd-shape diagnosis in both blueprints is corroborated by the code.
2. Both designs share the same first slab of work: split the workspace out of `BuildMounts`, add a `StartSpec`/`StartOptions` value object, route every `Backend.Start` call (enter, create, addstack, rebuild, reset, resize, snapshot, agent, start_recovery) through one lifecycle coordinator, deep-compare mount sets, and add an fd-headroom guard. That shared seam, and nothing about either broker engine, is what actually stops the bleeding.

The single most important structural asymmetry between the two designs: **Tier 2 keeps a real Linux kernel filesystem (ext4 on the guest disk) as the thing the agent's tools run on. Tier 3 replaces that real filesystem with a FUSE shim that must re-earn every POSIX guarantee over a network protocol.** Every judgment below follows from that.

## My independent correctness scorecard

Confidence is my confidence that the named tier is the safer *first writable implementation* (not the higher theoretical ceiling). "First pass" means the first AI-assisted implementation, not a matured one.

| Surface | Safer first pass | Confidence | Reasoning |
|---|---|---|---|
| Read-after-write | Tier 3 | Medium | Tier 3 is synchronous by construction. Tier 2 has no cross-endpoint read-after-write; `flush` is the barrier, so `host save; guest build` can race. But this only bites at the host/guest boundary. For a guest-centric agent (edit and build in the guest), Tier 2's guest disk is perfectly consistent, so the practical gap is narrower than the Tier 3 side implies. Tier 3's own host-originated visibility still rides FSEvents latency and cache invalidation, so it is not globally instantaneous either. |
| Editor atomic save | Tier 2 | High | Both endpoints are native filesystems under Tier 2; Mutagen stages a complete file and atomically relocates it (strong no-torn-file property). Tier 3's single host `renameatx_np` is the better *eventual* contract, but correct open-target identity, dual-dentry invalidation, replay, and error reporting make the first pass low confidence. Tier 3 overstates how mechanical this is. |
| Rename / exchange | Tier 3 contract, Tier 2 first-pass safety | Low (Tier 3 fidelity) | Only Tier 3 offers atomic cross-host exchange and `RENAME_NOREPLACE` without races. Tier 2 converges final content but models rename as delete+create and drops inode identity. Rare operation; low practical weight. Tier 3 wins only after differential and crash gates. |
| Unlink-while-open | Tier 2 | High | Within the guest this is native ext4 behavior under Tier 2, correct for free. Tier 3 must invent it with same-volume hardlink pins, identity verification, a journal, and cleanup, and it perturbs host link count and ctime. This is a surface where Tier 3 *adds* a silent-corruption hazard (handle retarget to a reused path) that both Tier 2 and even plain virtiofs avoid. |
| mmap (MAP_SHARED) | Tier 2 | High (practical) | Guest-local mmap is native ext4 under Tier 2; cross-endpoint divergence is a conflict, not corruption. Tier 3's writable shared mmap needs `FUSE_DIRECT_IO_ALLOW_MMAP` or a tested cached fallback, dirty-page ordering, and host-writer race handling. It is one of Tier 3's own "Critical" risks. Tier 3 materially overstates first-pass feasibility here. |
| fsync durability | Tier 2 for initial safety, Tier 3 only after proof | High (for Tier 2's stated boundary) | Tier 2 `fsync` is durable to the guest disk; convergence to the authoritative host copy needs an explicit successful flush. That must be stated in the contract, but it is not data-losing while the guest disk persists. Tier 3 promises the stronger boundary (host durability), but host bytes, guest-only metadata, pins, namespace changes, and a WAL are not crash-consistent just because each has an `fsync`. |
| Symlinks | Tier 2 | High (portable), Tier 3 more faithful | Tier 2 portable mode rejects escaping links (loud halt) and works for guest-local links. Tier 3 preserves raw link text and refuses escape on dereference (more faithful), but every path-taking opcode and rename race must use a beneath-root primitive; one miss is a security failure. Tier 3's first-pass containment confidence is overstated. |
| Perms / uid-gid / xattrs / hardlinks | Tier 2 (fails closed) | Medium | Tier 2 preserves only the exec bit and remaps ownership, and preflights/refuses hardlinks and material xattrs. Limited but honest and loud. Tier 3's metadata DB, ACL enforcement, link-count masking, setid rules, and timestamps are a second filesystem inside the filesystem; better contract, low first-pass confidence. Tier 2 understates how often mtime-sensitive builds, packaging, and generated code notice the loss. |
| inotify / watchers | Tier 2 for mainstream final-state workflows | Medium | Tier 2 applies real ops to a native guest FS, so guest watchers fire native inotify for both guest-originated edits and host-originated (applied) changes; the sequence is the staged apply, not the original syscalls, and it lags. Tier 3 correctly notes stock FUSE cannot synthesize arbitrary fsnotify, so full host-originated fidelity needs a custom guest-kernel patch (`FUSE_NOTIFY_FS_EVENT`). Paradox: for host-originated watches, first-pass Tier 2 (real applied events) is more likely to work than stock-kernel Tier 3. Tier 3 overstates watcher fidelity and hides a kernel-maintenance burden. |
| .git / guest VCS | Tier 3 | Medium (only via host-backed `.git`), Low (Tier 3's own bundle bridge) | Only a live broker can give native in-guest git. Tier 2's mandatory `.git` exclusion removes the repo database, index, refs, hooks, and history from the guest entirely. But see the crux below: correct in-guest git is *also* Tier 3's hardest first-pass workload, so this row is not the clean Tier 3 win it looks like. |
| Crash / detach | Tier 2 | High | Mutagen's three-way history, atomic staging, persistent guest disk, conflict state, and explicit flush are a small new recovery surface built on a mature engine. Tier 3 must recover dedup, uncertain namespace outcomes, pins, mappings, metadata, and event generations without retargeting handles. A WAL is necessary, not sufficient. Tier 3's 2-to-5-day confidence is substantially overstated. |
| Path-escape security | Tier 2 | High | A fixed Mutagen endpoint plus portable symlinks and canonical activation is the narrower attack surface. Tier 3 has a sound fail-closed design (Darwin resolve-beneath), but it exposes a large opcode parser and path resolver to a compromised guest and must win every symlink-swap and parent-rename race. Credible only after fuzzing and race auditing. |

Row tally: Tier 2 is the safer first pass in roughly 9 of 12 surfaces. Tier 3 clearly wins read-after-write and (nominally) `.git`, and holds the higher ceiling on rename/exchange and metadata. The decision cannot be a row count, because the two Tier 3 wins are the two most central to an in-VM coding agent. But it also cannot ignore that Tier 3's losing rows are *silent-corruption* rows, which is exactly what the primary criterion says to weigh most.

## Where Tier 3 overstates first-pass feasibility

- **mmap MAP_SHARED**: gated on a recent FUSE capability plus dirty-page and host-writer race correctness. Not mechanical.
- **Host-originated inotify**: requires shipping and maintaining a custom guest-kernel fsnotify ABI, rebased against every supported kernel. This is a distribution and security burden, not just code. Stock-kernel mode is explicitly degraded.
- **Unlink-while-open pins**: novel systems code with a catastrophic silent failure mode (handle retarget). "Finite and testable" undersells the crash-interleaving coverage problem.
- **Crash / WAL / O_TMPFILE**: crash-consistency across every journal transition is combinatorial; fault injection is evidence, not proof.
- **Path-escape**: correct on paper with Darwin resolve-beneath, but first-pass confidence on *every* opcode under concurrent host renames is overstated.
- **Schedule**: "2 to 4 elapsed hours" for Phase 0 and "2 to 5 days" total are not credible for validating vsock relay ownership through Colima, resolve-beneath across volumes, direct-I/O mmap, a kernel event patch, and pin behavior. The prior Codex verdict already pushes back on this, correctly.
- **In-guest git (my addition, see crux)**: presented as a Tier 3 benefit, but it is one of the hardest workloads to make *correct* on a first-pass live broker.

## Where Tier 2 understates workflow breakage

- **`.git` in the guest**: the doc calls it "the largest product tradeoff." It is larger than that for the stated use case: agents routinely run `git status`, `git diff`, `git log`, `git blame`, and commit inside their working directory. With `.git` excluded, none of that works in the guest. This is a release-defining boundary, not a footnote. (It is, however, a *loud* limitation, which matters below.)
- **Cross-endpoint stale read**: an interface that implies a live workspace but silently serves a pre-flush guest copy to a build is a real, quiet trap.
- **Stale-buffer overwrite**: a stale editor buffer saved on one side can overwrite a newer synchronized change, and `two-way-safe` cannot distinguish that from an intentional revert. (Partially credited: a live filesystem has the same stale-buffer hazard; this is not uniquely a Tier 2 flaw, but Tier 2's replication framing makes it easier to hit unknowingly.)
- **Mandatory `build`/`target` exclusion**: collides with repositories that legitimately use those names for source.
- **`.gitignore` to Mutagen translation**: must be exactly `git check-ignore` equivalent. A partial parser can silently sync a secret that git ignores, or silently drop a needed file. This is a silent-correctness risk *inside* Tier 2 that the doc treats mostly as an implementation task. It is testable against `git check-ignore`, but it is real.
- **Distribution**: Mutagen is pre-1.0 and its official binaries carry SSPL from v0.17. This is a loud, hard packaging blocker, not a silent one, but it can stall shipping.

## The crux

### Is Tier 2's `.git` + stale-read + metadata loss acceptable, or disqualifying?

- **Metadata loss: acceptable.** For ordinary source trees, exec-bit plus loud preflight rejection of hardlinks and material xattrs is fine. Git tracks content, exec bit, and symlinks, all of which Tier 2 carries. The failure mode is loud (refuse/warn), not silent. The exceptions (mtime-driven builds, packaging repos, linked fixtures) must be rejected explicitly, and the doc mostly does this.
- **Stale read: workflow-dependent, mostly acceptable.** For a guest-centric agent it is a non-issue. It becomes a trap only when the UI implies a live filesystem across the host/guest boundary. Fixable with honest labeling and flush-barrier discipline.
- **`.git` exclusion: severe, and the real decision axis.** It is disqualifying for a *transparent* "coding inside an isolated VM" product. It is *not* disqualifying for a "guest editing plus host-side VCS" product, which is a real, shipped pattern in sync-based remote dev. Crucially, `.git` absence is **loud**: the agent runs git, gets an error or empty output, and the limitation is obvious on day one. That is the opposite of the silent corruption the primary criterion prioritizes against.

### Is Tier 3's silent-corruption risk containable by Phase 0 kill-gates plus differential tests?

Partly. Phase 0 genuinely contains the *feasibility* risk: vsock relay ownership, resolve-beneath availability, direct-I/O mmap, and safe event injection are binary go/no-go checks, and killing early on any of them is sound. Differential testing against a native ext4 reference is the right oracle and catches most functional divergence in namespace, bytes, metadata, and error codes.

What it does not contain on a first pass, in days: rare TOCTOU race windows, crash-consistency across every interleaving, host-originated watch completeness under FSEvents coalescing and overflow, and MAP_SHARED dirty-page races. These pass demos and fail in production, as the Tier 3 doc itself concedes. So the risk is containable as a multi-week staged program with brutal gates, not as a two-week deliverable.

### My key independent insight: the `.git` argument cuts both ways

The case for preferring Tier 3 rests almost entirely on in-guest git. But git is the single filesystem client most likely to be *silently corrupted* by a first-pass live broker, because git leans on exactly Tier 3's weakest surfaces:

- The git index keys "has this file changed" on precise `stat` fields: `ino`, `mtime`, `ctime`, `size`, and device. Tier 3 allocates broker inode IDs per object generation, derives `ctime` from host metadata, and treats `mtime` as best-effort. If any of those drift or disagree across reconnects, git either re-hashes everything (slow but safe) or, in the bad case, misses a change (silent, wrong). Git's own "racy git" mitigation exists precisely because small stat inconsistencies cause silent staleness.
- Packfiles are `mmap`'d (MAP_SHARED read paths), the surface Tier 3 rates Critical.
- Ref updates are atomic rename-over-open, and object writes use fsync for durability, both first-pass-risky under Tier 3.
- `.git` is a dense small-file, high-metadata tree, the worst case for a per-op vsock round trip.

So "Tier 3 gives you git" is only true once Tier 3 has gotten mmap, stat-identity, atomic rename, and fsync durability right, which the prior verdict itself rates Low to Medium first-pass. A first-pass Tier 3 pointed at a real repo risks *silently wrong* git (index corruption, missed changes, torn packfiles), which is worse than Tier 2's *loudly absent* git. Meanwhile Tier 2 leaves the user's real, complete git repository intact on the host. Using git as the decisive reason to front-load Tier 3 is therefore partly self-undermining.

## Agreement and dissent with the Codex judge

I read `docs/design/fs-broker-verdict.md` after forming the above. It is a strong, unusually honest document. My position: **agree with roughly 90 percent of its analysis, dissent on its headline and its decisive weighting.**

Where I agree:

- Its scorecard nearly matches mine: Tier 2 is the safer first pass on almost every byte-level surface; Tier 3 wins read-after-write and `.git`. Good faith, well reasoned.
- Shared seam first. Tier 3's schedule is optimistic; "2 to 4 hours" is not credible. The kill-gates (transport/identity, 1M-race containment, FUSE capability, watch feasibility, pin feasibility, descriptor bound, differential, VCS-read, atomic-save, WAL/crash, durability, mapping, metadata, git-mutation) are excellent and I would keep them essentially verbatim.
- Capability honesty (never return success as a placeholder) and differential release gates are the correct containment methodology.
- Tier 2 must ship only as an explicit "synchronized copy," never as "local filesystem equivalence."

Where I dissent:

1. **Headline.** The verdict names "Tier 3 Phase 0 first" the winner, yet its own plan spends days 3 to 14 trying to *kill* Tier 3 and ships Tier 2 if any gate flips. That is not "Tier 3 first"; it is "de-risk Tier 3 cheaply, default to Tier 2." Given the prompt's primary criterion (silent breakage and data loss) favors Tier 2 on 9 of 12 surfaces, and the secondary criterion (days, not weeks) is only met by Tier 2 producing a shippable product, I name **Tier 2 the implementation to ship first**, with **Tier 3 Phase 0 run in parallel as a gated spike** that may supersede Tier 2 later. Concretely: put Tier 2 on the critical path, not on the "if capacity allows" branch.

2. **Decisive weighting of `.git`.** The verdict lets one factor override its own 9-row scorecard. I dissent on two grounds. First, `.git` absence is *loud*, and the primary criterion is *silent* breakage and data loss; a live broker's first-pass failures (silent mmap loss, missed watches, pin retarget, silently wrong git index) are precisely the silent failures we are told to weigh most. Second, as shown above, correct in-guest git is Tier 3's hardest deliverable, so "git" is a weak reason to *trust* Tier 3 first. The verdict reaches for a third option (expose host `.git` live through the broker, keep it visible in the read-only slice) that is neither tier's stated design and that only pays off once the whole live broker is correct.

3. **Same-family bias flag (mild, mostly controlled).** The bias I detect is not sloppiness; the document is rigorous. It is that a same-family judge elevated the more ambitious same-family design on the strength of a single benefit (`git`) without pricing in that this benefit is the last thing that design reliably delivers, and it labeled a fundamentally Tier-2-defaulting plan as a Tier-3 win. The honest, criterion-aligned framing is: Tier 2 is the first shippable, correctness-safe product; Tier 3 is a gated research successor.

I do **not** think the Codex verdict is wrong about the destination. If Tier 3's gates pass and in-guest git proves both necessary and correctly deliverable, Tier 3 is the better long-term architecture. I disagree about what you *build and ship first* under the stated criteria.

## Verdict

Implement in this order.

1. **Shared seam (both designs need it, and it fixes the actual bug).** `StartSpec`/`StartOptions`, `BuildSupplementalMounts` with the workspace removed, one lifecycle coordinator that owns every start callsite, deep mount comparison, the `kern.num_files`/`kern.maxfiles` headroom guard, supplemental cardinality caps, and a Lume decision (support repeated `--shared-dir` or reject unsupported combinations, never silently take `mounts[0]`). A static check must prove no command-layer path can reconstruct a workspace mount. This alone eliminates ENFILE regardless of which broker wins.
2. **Tier 2 on the critical path**, shipped as an explicit "synchronized copy," because it is the correctness-safe, days-scale path to a working agent dev experience.
3. **Tier 3 Phase 0 in parallel as a gated spike**, using the Codex kill-gates. Promote Tier 3 above Tier 2 only if every Phase 0 gate passes and real testing shows Tier 2's `.git` gap is both unacceptable in practice and correctly closable by Tier 3.

### First two weeks

Days 1 to 2: shared seam behind tests and a canary. Build the trace/oracle corpus (Cursor/VS Code save, Vim save, git status/commit, Node and Go watchers, rename-over-open, unlink-open, shared mmap, fsync) for use by both tracks.

Days 3 to 5 (critical path, Tier 2): project resolver, stable IDs, guest paths, mandatory filters, and the `.gitignore` compiler validated against a `git check-ignore` conformance corpus (the correctness-critical piece). Pin the Mutagen version with templated JSON and golden tests; generate private SSH wrappers. Manual single-project attach measuring fd plateau through recursive traversal.
Days 3 to 5 (parallel, Tier 3 Phase 0): run the five flip gates below. Record pass/fail per seam. Do not let this delay the Tier 2 path.

Days 6 to 10 (critical path, Tier 2): activation daemon, leases, idle detach and idle VM stop, `cloister open` for shell, Claude, and Codex; guest-path integration (bashrc, vmconfig, agent compose binds). Real single-project e2e: atomic saves, guest inotify with a real dev server, host-edit propagation, non-destructive conflict, crash/reconnect, and a two-VM fd soak. Decide the git story for v1: document host-side git, and scope a narrow, read-mostly in-guest git option only if it can be added without the full bespoke FS.
Day 10 gate review: evaluate Tier 3 Phase 0. If all seams passed cleanly and the git gap is proving unacceptable, greenlight Tier 3 Phase 1 (read-only slice) as the successor track. Otherwise Tier 2 ships as default and Tier 3 remains research.

### Exact kill-gates that would flip the decision toward Tier 3 first

These are the conditions under which I would move Tier 3 onto the critical path ahead of Tier 2 (I adopt the Codex gates, which are excellent):

- **Transport and identity**: one hour under load and 100 forced reconnects with zero duplicated committed mutation IDs, zero cross-profile attachment, and 100 percent rejection of expired, replayed, wrong-profile, and wrong-epoch tokens. If the Colima relay cannot be owned and restarted without patching an unstable private hypervisor interface, do not flip.
- **Containment primitives**: the supported macOS and target volume must provide root-relative, no-follow single-path and dual-path operations (or an audited fd-walk alternative already exists). At least 1,000,000 concurrent traversal, symlink-swap, parent-rename, and root-replacement attempts reach zero outside sentinels. Any escape blocks the flip.
- **FUSE capability**: the target kernel and library negotiate the needed opcodes and invalidations without silent degradation. Missing both direct-I/O shared mmap and a differential-tested cached fallback blocks the flip.
- **Watch feasibility**: for 100 repetitions of host create, atomic replace, rename, delete, subtree move, and a 10,000-event burst, each representative Node, Go, and editor watcher observes a usable event and the correct final state, or receives overflow and demonstrably rescans. One silent missed final-state rebuild after the broker reports healthy blocks the flip.
- **Pin feasibility**: 100,000 open/rename/replace/unlink/reconnect/close cycles produce zero handle retargets, zero premature deletion, and zero leaked pins after recovery, with no ctime churn that breaks target tools.
- **Descriptor bound**: growing a fixture from 20,000 to 1,000,000 visible files, 200,000 guest handles, and recursive watches adds no more than a small fixed number of persistent host descriptors and returns to baseline after detach.
- **VCS-read and (my addition) VCS stat-identity**: unmodified guest `git status`, `git diff`, root discovery, ignore evaluation, and history reads work on a representative repo, *and* the git index remains stable across a broker reconnect (no spurious "everything modified" and, critically, no silently missed change) because `ino`/`mtime`/`ctime`/`size` are consistent. If in-guest git cannot be made both correct and stable, the single biggest reason to prefer Tier 3 evaporates.

Conversely, the conditions that keep Tier 2 first (any one suffices): any Phase 0 seam fails, any differential run shows a silent namespace/content/metadata/error mismatch, the git stat-identity gate is not met, or the guest-kernel watch patch cannot be shipped and maintained safely.

## Bottom line

Winner to implement and ship first: **Tier 2 (Mutagen sync broker), on top of the shared mount-and-lifecycle seam that actually fixes the fd bug, with Tier 3 Phase 0 run in parallel as a hard-gated research spike.** I **dissent from the Codex judge's headline** ("Tier 3 Phase 0 first") while agreeing with almost all of its underlying analysis, scorecard, and kill-gates; its own plan is really a Tier-2-defaulting plan wearing a Tier-3 label. The single most important reason is that the primary criterion is *silent* breakage and data loss, and on that axis Tier 2 is safer on nine of twelve surfaces because the agent's tools run on a real kernel filesystem, whereas Tier 3 re-earns every guarantee with silent-corruption failure modes, and its one decisive advantage, in-guest git, is precisely the workload most likely to be silently corrupted by a first-pass live broker, so it is a poor reason to bet the first two weeks on the bespoke filesystem.
