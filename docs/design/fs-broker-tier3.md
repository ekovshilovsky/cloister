# Tier 3 bespoke filesystem broker

Status: implementation blueprint, read-only architecture pass

Scope: one live, writable project per active Cloister profile

Primary target: Colima and Lima with a Linux guest

Secondary target: Lume only after a macOS guest adapter reaches the same contract

License for new Cloister-owned broker code and protocol definitions: proprietary

## Executive decision

Build a host-side broker, `cloister-fsd`, and a guest-side low-level FUSE client, `cloisterfs`, connected through a profile-bound virtio-vsock relay. The host broker exports one authorized project root. It resolves every request beneath that root, opens the minimum host descriptors needed for the operation, performs the operation, and closes those descriptors before replying. It never keeps a host file or directory descriptor for a guest inode, lookup, open handle, or watch.

The motivating failure is a resource-shape bug, not merely a large limit: one current VM retained about 176,000 host files and another retained about 39,000, together consuming about 91 percent of `kern.maxfiles=491520`. Raising limits preserves the same unbounded relationship. Tier 3 changes that relationship.

This is the recommended Tier 3 mechanism because it provides the control points the problem actually requires:

1. Project scope is an attachment capability created from a canonical host project path. The guest cannot submit or widen that path.
2. Activation is independent of VM startup. A project can attach when Claude, Cursor, or Codex opens it, then detach when the last routed session closes.
3. The broker protocol can carry Linux filesystem operations without depending on the host hypervisor's directory sharing implementation.
4. Filtering happens before host lookup or enumeration. Filtered trees can instead be served from guest-local storage.
5. Host descriptors are bounded by transport connections and in-flight operations, not by the number of files, inodes, open guest handles, or watches.

Virtiofs is already FUSE over virtio. Merely replacing Apple's virtiofs implementation with another FUSE server would not supply the required lifecycle, filtering, and descriptor discipline. A custom virtiofs server also requires changes inside the Virtualization.framework VM owner, and Apple exposes a configured directory share rather than a supported external virtiofs daemon endpoint. A macFUSE or NFS re-export adds another cache and identity translation layer, still leaves remote change notifications incomplete, and makes path containment harder to audit. The guest-side FUSE approach places the Linux VFS boundary where Cloister can control it and keeps the backend-specific work limited to a byte-stream relay.

This design does not claim that stock FUSE already provides exact remote watch delivery. It does not. Linux FUSE defines invalidation notifications, but no notification that asks the kernel to generate arbitrary fsnotify events. General availability therefore requires a narrow guest-kernel FUSE extension for host-originated inotify and fanotify notifications. A stock-kernel compatibility mode may provide cache invalidation and a conservative rescan signal, but it must be labeled degraded and must not pass the full-correctness release gate.

## Goals, contract, and exclusions

### Required behavior

- Expose exactly one canonical project root to a profile at a stable guest path, writable by the VM user.
- Preserve normal Linux application behavior for editor saves, builds, Git working-tree access, memory mapping, metadata, links, renames, watches, and crash recovery within the documented cross-system consistency model.
- Keep every unfiltered host path outside the project inaccessible through this data plane.
- Make `node_modules`, `.git`, and configured build outputs unavailable to host traversal by the broker, with optional guest-local backing.
- Keep the existing small supplemental shares separate. SSH and GPG remain read-only. Existing headless restrictions on Claude extension mounts remain in force.
- Bound host descriptors independently of project size and watcher count.
- Fail explicitly when the backing volume cannot support a correctness mechanism. Never silently fall back to broad writable virtiofs.

### Important boundaries

- The host is trusted, as in Cloister's current threat model. The broker protects the host from a compromised guest, not from a malicious host process.
- The exported filesystem has network-filesystem style coherence for unsynchronized cross-host and guest writers. Syscall ordering, atomic namespace operations, durability calls, and append atomicity are honored. Unsynchronized writes to the same byte range, especially through concurrent writable mappings, have last-completing-write behavior and are a data race.
- Advisory byte-range locks are coherent among guest clients of one attachment. An unrelated host process can ignore advisory locks today, and an fd-free broker cannot retain a host `fcntl` lock for a guest handle. This limitation is surfaced in capability reporting.
- Host-originated event history is limited by macOS FSEvents coalescing and overflow behavior. The guarantee is eventual cache invalidation and a conservative event or overflow indication that makes conforming dev tools rescan. Guest-originated event sequences are exact because the Linux VFS emits them at the originating syscall.
- Filtered paths are deliberately outside the host-backed correctness domain. A guest-local overlay provides native guest filesystem semantics, but its contents are not a live mirror of the hidden host subtree.

## Repository findings and required seam changes

The current mount path is centralized enough to replace, but several consumers assume that a host path is also the guest path.

| Area | Current behavior | Tier 3 change |
|---|---|---|
| `internal/vm/mount.go` | `BuildMounts` always prepends `workspaceDir` as writable, then adds policy-filtered supplemental mounts. | Split workspace exposure from `BuildSupplementalMounts`. In broker mode, do not put the project in `[]vm.Mount`. Preserve the existing supplemental catalog and permissions. |
| `internal/vm/backend.go` | `Backend.Start` takes resources, `mountInotify`, and `[]Mount`. | Replace the positional growth with `StartOptions`, or add a separate optional `BrokerTransport` capability. VM startup receives only persistent supplemental mounts. Broker attach is a separate lifecycle. |
| `internal/vm/colima/backend.go` | Starts `vz` with `virtiofs`, passes all mounts, and passes `--mount-inotify` explicitly. | Continue virtiofs only for small supplemental mounts. Keep `mountInotify=false`. Arrange a profile-bound vsock-to-host-Unix-socket relay for the broker. Do not add the project to Colima mounts. |
| `internal/vm/lume/backend.go` | Passes only `mounts[0]` to `lume run --shared-dir`; the rest are ignored. | Do not assume that removing the first workspace mount makes supplemental mounts safe. Tier 3 must be rejected for Lume until Lume can relay the broker and the macOS guest adapter is complete. Separately fix or explicitly model Lume's single-share limitation before enabling supplemental shares. |
| `internal/config/config.go` | `Profile.StartDir` is the workspace source, `ResolveWorkspaceDir` canonicalizes it, `MountPolicy` governs supplemental resources, and `MountInotify` defaults false. | Add `workspace_mode: broker`, filter policy, and optional path routing. Keep `StartDir` as the default project selector. Deprecate `MountInotify` for broker data, since broker watch delivery is per attachment. |
| Guest config and shell | `vmconfig.Workspace` and the bashrc contain the absolute host path. `~/workspace` points at the pass-through mount. | Set `Workspace` to `/home/<host-user>.guest/workspace`. Do not reveal the canonical host root. Mount the active attachment directly at the stable path. |
| Agent containers | Compose and Docker arguments bind the host-shaped workspace path from inside the VM. | Bind the stable guest broker mount into the container. A container must not receive or reconstruct the host path. |

`VMHome` already defines the Linux guest home as `/home/<host-user>.guest`. The stable mount should therefore be `/home/<host-user>.guest/workspace`. An internal staging mount such as `/run/cloister/projects/<attachment-id>` can be bind-mounted to that path during attach, but user-facing tools should see only `~/workspace`.

Every `Backend.Start` call site must be audited, not just the four named flows. The repository currently constructs mounts in create, enter, add-stack, rebuild, reset, resize, and snapshot paths. `setup_openclaw` also starts a VM. A migration is incomplete if any of these can recreate the broad project virtiofs mount.

### Command integration

#### `cmd/enter.go`

1. Resolve profile and start the VM with supplemental mounts only.
2. Start or locate `cloister-fsd` under the user's launchd session.
3. Attach the profile's resolved `StartDir` and wait for the guest mount health check.
4. Start tunnels, set terminal identity, and SSH with the working directory set to `~/workspace`.
5. Hold a session lease until SSH exits. On final lease release, drain, `syncfs`, unmount, and detach.

If the VM is already running, attach remains a separate operation and needs no restart. Multiple sessions for the same project increment a reference count. A different project cannot replace an active attachment in that profile.

#### `cmd/addstack.go`

`MountsChanged` currently relies on `BuildMounts` only appending entries and compares lengths. Replace this with a typed diff of persistent supplemental mounts. A changed filter policy or project path is broker state and does not require a VM restart. A new supplemental mount, such as the Ollama model store, still can. Before any required stop, suspend new broker requests, drain active sessions or ask for confirmation, detach, restart, then reattach.

#### `cmd/rebuild.go`

Detach before backup, stop, delete, snapshot, or clone operations. Start the rebuilt VM without a project share, provision the guest daemon and kernel capability, then attach only for steps that need project access. The Colima path must validate the FUSE protocol version and host relay. The Lume path must return a clear unsupported error for broker mode until its adapter passes parity tests. Never fall back to `--shared-dir` without explicit user selection of a lower tier.

#### `cmd/repair.go`

Repair the broker as an independently checkable subsystem: host daemon version, control socket ownership, relay, guest service, kernel event ABI, stable mountpoint, and stale attachment recovery. Repair does not need to expose a project merely to reinstall services. If an attachment is active, repair must not remount beneath running tools without an explicit drain.

## Architecture

```text
Host application launcher
  -> path router and lease manager
     -> VM lifecycle through existing Backend
     -> cloister-fsd control socket, mode 0600
        -> attachment capability for one canonical project root
        -> FSEvents stream and metadata reconciler
        -> bounded filesystem worker pool
        -> durable operation and handle journal
             || profile-bound virtio-vsock byte stream
             || backend relay owned by Lima or Lume
        -> cloisterfs guest daemon
           -> /dev/fuse low-level request loop
           -> guest-local filtered subtree store
           -> cache invalidation and fsnotify event injection
           -> /home/<user>.guest/workspace
                -> Claude, Cursor, Codex, shells, and containers
```

### Host components

`cloister-fsd` is a per-user launchd daemon, not root. It owns:

- A mode `0600` control socket for the Cloister CLI.
- An attachment registry keyed by profile, VM identity, project ID, and session epoch.
- A bounded worker pool for filesystem RPCs.
- One FSEvents stream per active attachment, plus an in-memory metadata snapshot. The snapshot can be proportional to visible paths, but it contains no open file descriptors.
- A journal under `~/.cloister/broker/` for request deduplication, object metadata, open-handle pins, temporary files, and recovery state.
- A per-volume private state directory for same-volume hardlink pins and unnamed temporary files. Activation fails if no private directory on the source volume can be created safely.

The daemon must not run with Full Disk Access as an installation shortcut. It can expose only what the invoking user can access.

### Guest component

`cloisterfs` runs as a systemd service in the Linux guest. The mount itself uses `allow_other` only if container access requires it, together with `default_permissions` and an explicit allowed guest UID. The daemon:

- Implements the low-level FUSE protocol, including `RENAME2`, `TMPFILE`, `FSYNC`, `FSYNCDIR`, `FALLOCATE`, locks, xattrs, and `SYNCFS` when negotiated.
- Routes filtered path prefixes to a persistent or ephemeral guest-local backing root instead of the host broker.
- Reconnects using the attachment ID, epoch, and last completed request sequence after a broker restart.
- Applies host invalidations to the FUSE cache.
- Uses the Cloister FUSE fsnotify extension for host-originated events in full-correctness mode.

Use a mature low-level FUSE library only after an opcode and behavior audit. Missing `FUSE_TMPFILE`, direct-I/O mmap support, `RENAME2`, or unsolicited invalidation is a blocker, not a reason to emulate success incorrectly. The host remains Go-compatible, but a small C or Rust guest daemon is acceptable if it materially reduces FUSE ABI risk. Any new Cloister-owned source is proprietary.

### Transport

The data plane is a versioned, length-prefixed binary protocol over a reliable ordered stream. It needs request IDs, attachment epoch, operation ID, credentials, inode and handle IDs, flags, offsets, deadlines, status, and bounded data chunks. Namespace mutations carry idempotency keys. Negotiation advertises protocol version and capabilities such as `TMPFILE`, `DIRECT_IO_ALLOW_MMAP`, `FSNOTIFY_REMOTE`, `RENAME_EXCHANGE`, ACL storage, and sparse-hole operations.

The preferred transport is virtio-vsock. Apple's Virtualization framework makes a `VZVirtioSocketConnection` a byte-stream file descriptor, but the process that owns the VM also owns its socket listener. Colima does not currently give Cloister that object directly. Tier 3 therefore requires a small Lima host-agent relay from a dedicated guest vsock port to `cloister-fsd`'s per-profile Unix socket. Lume requires the equivalent relay. For an engineering spike only, an authenticated SSH or host-NAT transport can validate filesystem semantics, but it is not the performance target.

Mutual authentication is still required even on vsock. The CLI gives the guest a single-use random attachment token through the authenticated SSH provisioning channel. The host binds it to VM identity, profile, project ID, epoch, expiration, and read-write policy. A guest path is never part of authorization.

### Control and data contracts

The host control API should remain small and local:

```text
EnsureAttachment(profile, canonicalRoot, filterPolicy, cachePolicy) -> attachmentID, epoch
AcquireSession(attachmentID, app, relativePath, callerPID) -> sessionID, oneTimeGuestToken
ReleaseSession(sessionID, force) -> detached, blockers, durabilityStatus
AttachmentStatus(profile) -> state, capabilities, counters, health
RecoverAttachment(attachmentID) -> recoveryReport
```

Only the trusted CLI can call `EnsureAttachment` with a host path. The data-plane guest starts with `HELLO(attachmentID, epoch, token, protocolRange)`, receives the root object and capabilities, then sends filesystem requests using opaque node and handle IDs. An attachment accepts only one active connection generation. Reconnect replaces a connection only after proving the same epoch and replay position.

Cancellation has defined semantics. A canceled read or stat may be abandoned. A namespace mutation that reached the host finishes under its operation ID and is recoverable through replay. The broker never reports cancellation when it has already committed a mutation without also returning its committed result on retry.

## Per-path activation and detachment

The user-facing operation is conceptually:

```text
cloister open --app claude /host/path/to/project/file
cloister open --app cursor /host/path/to/project
cloister open --app codex /host/path/to/project
```

GUI integrations can invoke the same control operation through a Finder service, editor extension, or URL handler.

### Resolution

1. Canonicalize the requested existing host path without following a final symlink outside an authorized root. If it is a file, retain its project-relative path.
2. Choose the project root from an explicit route first, then the nearest recognized project marker, then the directory itself. A `.git` file used by worktrees is a marker even when `.git` will later be filtered.
3. Select a profile by an explicit route or longest matching configured `StartDir`. If selection is ambiguous, ask once and store a private host-side route. Do not write personal route data into a shared repository.
4. Confirm that the canonical root is not `/`, the user's home, or another configured broad parent unless the user explicitly created that exact project route.
5. Calculate a stable project ID from a keyed hash of volume identity and canonical root. Do not expose the host path to the guest.

### Attach

1. Acquire a profile and project lease. Reject a different active project for that profile.
2. Ensure the VM is running without a workspace virtiofs mount.
3. Start the host attachment, its journal, FSEvents stream, and initial snapshot. Start events before scanning so no host mutation falls between scan and watch activation.
4. Pass the one-time capability to the guest, mount at `~/workspace`, and verify a challenge file through metadata and content RPCs.
5. Translate the originally requested host path into `~/workspace/<relative-path>` and launch the chosen app or SSH session there.

### Detach

The launcher watches the routed application process, not a persistent Cursor server or the VM itself. Each launched process runs in a guest systemd scope so descendants and open handles are attributable.

On close, the lease manager:

1. Decrements the project reference count. It does nothing while another routed session uses the project.
2. Stops the closing session scope and waits for its descendants to exit.
3. Blocks new opens, waits for in-flight operations, flushes dirty mappings, issues `syncfs`, and sends FUSE `DESTROY`.
4. Unmounts normally. It never uses a lazy unmount as the successful path.
5. Commits the journal, removes open-handle pins and abandoned temporary files, stops FSEvents, revokes the capability, and reports detach complete.

If unrelated guest processes still hold the mount, detach reports the PIDs and remains attached for a short configured grace period. Forced detach first syncs and then returns explicit I/O errors to remaining users. It cannot be described as seamless or clean.

For a headless profile, the agent runtime is the opener. `agent start` acquires the session lease before starting the native process or container, and `agent stop` performs the same drain and detach sequence. An idle timeout cannot detach while the agent runtime or its container still owns the lease.

## Object and path model

### Stable identities without stable host descriptors

The guest sees stable inode and file-handle IDs allocated by the broker. A broker object record contains:

- Project ID and attachment epoch.
- A random object ID.
- Current relative names known for the object.
- Host volume ID, inode number, birth time, type, and generation observations.
- Guest metadata that cannot be represented directly on the host.
- Open-handle reference count and the path of any same-volume pin.

On the first guest open of a regular host file, the broker creates a hardlink in its private same-volume pin directory and verifies that its identity matches the opened source. The pin preserves the inode across host or guest unlink and rename without holding a descriptor. All later operations for that open object resolve the private pin, open it, perform one syscall or bounded compound syscall, and close it. The last release removes the pin after all acknowledged writes are durable to the level requested by the guest.

Pins temporarily increase the host inode's link count. Guest `st_nlink` subtracts broker-owned pins. This is visible to unrelated host software that inspects link counts, and it is an unavoidable external artifact of preserving unlink-while-open without retaining a descriptor. Volumes that do not support same-volume hardlinks cannot offer this contract and must fail writable activation.

Directories cannot be hardlink-pinned. Guest namespace mutations maintain directory object paths transactionally, and broker-originated renames update descendants lazily through parent object relationships. An external host removal of an open directory may make a later directory-handle operation return `ESTALE`, as on a network filesystem. This case must never retarget the handle to a new directory that reused the path.

### Contained resolution

For every request, open a fresh root descriptor, verify the configured root identity, and resolve relative to it. On supported macOS releases use Darwin's `O_RESOLVE_BENEATH`, `AT_RESOLVE_BENEATH`, xattr resolve-beneath flags, and `RENAME_RESOLVE_BENEATH`. Use no-follow flags for operations that address the link itself. Close the root and operand descriptors before replying.

Internal relative symlinks may resolve. An absolute symlink or a relative symlink that would leave the project can exist and can be read with `readlink`, but dereferencing it returns `EXDEV` or `EACCES`. The broker never cleans a path string and then performs an unrestricted path syscall. If required resolve-beneath primitives are unavailable, Tier 3 fails closed unless a separately audited component-by-component fd walker is enabled.

Hardlinks are permitted only when both source and destination resolve within the same attachment. Directory hardlinks and links to the private state directory are rejected.

## Filesystem correctness surface

The following table is the minimum acceptance contract. An unsupported operation returns the accurate error, generally `EOPNOTSUPP`, and is omitted from negotiated capabilities. It is never acknowledged as a no-op unless POSIX permits that behavior.

| Surface | Required implementation |
|---|---|
| Lookup and negative lookup | Path-relative, contained lookup. Stable node ID while referenced. Negative cache timeout is zero in strict mode. Host create invalidates the parent entry immediately after event reconciliation. |
| Create and exclusive create | Use `openat` beneath a held parent with `O_CREAT`, requested mode, and `O_EXCL` when supplied. Apply guest umask in the guest kernel, not twice. Return the created identity from `fstat` before closing. |
| Editor atomic save | Named temp files are ordinary files in the target directory. Write, flush, then rename-over is atomic through `renameatx_np`. Preserve or intentionally replace mode and xattrs according to the editor's actual calls. Invalidate both old and new dentries after commit. |
| `O_TMPFILE` | Require FUSE protocol 7.37 or newer. Create a randomly named, mode `000` file in the private same-volume state directory, expose only an unnamed object ID, and apply the requested ownership and mode before atomically linking it into the project on the corresponding FUSE link operation. Release deletes an unlinked temporary. Journal every state transition. |
| `fsync` and `fdatasync` | Open the pinned object for the operation, call host `fsync` or `fdatasync`, propagate the real error, and close. `fsync` also durably records pending guest-only metadata. A strict durability option uses `F_FULLFSYNC` on supported local volumes. `FSYNCDIR` and namespace transactions use directory sync where supported plus the broker WAL. |
| Rename-over | One host atomic rename syscall, never copy plus delete. An open overwritten target remains accessible through its pin. Update object names only after the syscall succeeds. |
| `RENAME_NOREPLACE` | Map to `renameatx_np` with `RENAME_EXCL` and resolve-beneath flags. No check-then-rename race. |
| `RENAME_EXCHANGE` | Map to `renameatx_np` with `RENAME_SWAP` and resolve-beneath flags. If the source volume lacks the capability, return `EOPNOTSUPP`. Never emulate with three visible renames. |
| Directory rename | Use one atomic host rename. Serialize conflicting guest namespace mutations with ordered parent-object locks. Descendant object handles refer through the renamed parent identity. External host races resolve to one real host result or `ESTALE`, never a mixed tree. |
| Hardlinks | Map FUSE `LINK` to `linkat` after both endpoints pass containment and identity checks. Report a stable shared inode and correct guest link count after subtracting pins. Reject cross-volume links with `EXDEV`. |
| Symlinks | Store the supplied link text unchanged when the host accepts it. `lstat` and `readlink` address the link. Follow only through beneath-root resolution. Escaping links remain visibly broken from the guest. |
| Unlink while open | The same-volume hardlink pin is the silly-rename equivalent. Remove the project name atomically, keep the object through its private pin, and delete the pin on final release. Open readers and writers continue to address the original inode. |
| Open flags | Honor access mode, `O_TRUNC`, `O_EXCL`, `O_NOFOLLOW`, `O_DIRECTORY`, `O_SYNC`, `O_DSYNC`, and append semantics. Unknown correctness-affecting flags fail instead of being dropped. |
| Read and write | Use `pread` and `pwrite` on a descriptor opened only for that RPC. Short I/O and host errors propagate. Request size is bounded and large requests can be split without changing the visible byte count or error rule. |
| `O_APPEND` races | Serialize append decisions by object in the broker and perform each write with a host descriptor opened `O_APPEND`. This preserves atomic end-position selection against guest appends and cooperating host `O_APPEND` writers. Do not implement append as `stat` plus `pwrite`. |
| Truncate | `ftruncate` the pinned object for open-handle truncate, or contained `truncate` for path operations. Serialize against guest writes and invalidate page cache beyond the new end before replying. Preserve sparse behavior when extending. |
| Sparse files | Reads from holes return zeros. Use host sparse writes naturally. Implement `SEEK_DATA` and `SEEK_HOLE` when the host volume supports them. Map punch-hole and zero-range operations only when a host primitive preserves semantics, otherwise return `EOPNOTSUPP`. Test allocated block counts, not only content. |
| `mmap`, private | Normal FUSE cached or direct-I/O mapping rules apply. `MAP_PRIVATE` changes never reach the host. Reads observe the attachment consistency policy. |
| `mmap`, shared | Require `FUSE_DIRECT_IO_ALLOW_MMAP` or a tested cached-mode fallback. Dirty pages are sent as FUSE writes. `msync(MS_SYNC)` and unmap errors reach the application, and `fsync` waits for prior mapping writes. Host change invalidation must not discard dirty guest pages. Conflicting unsynchronized host writes are a documented data race. |
| File and record locks | Implement `GETLK`, `SETLK`, `SETLKW`, and `flock` in the broker keyed by object and lock owner, with disconnect cleanup and deadlock-safe waits. Coherence with unrelated host advisory lockers is not promised because retaining the corresponding host lock would require a retained fd. |
| Directory iteration | Open the directory only for one `READDIRPLUS` operation, use bulk metadata enumeration where available, remove filtered names, return opaque continuation cookies, and close. A cookie can return `ESTALE` after a concurrent external directory mutation. No duplicates or skipped entries are allowed without such an explicit restart signal. |
| Permissions and mode | Present owner and group as the configured guest user and primary group. Evaluate guest mode bits plus host-user access, with host access as the upper bound. `chmod` changes host POSIX mode when representable. Clear setuid and setgid on writes according to Linux rules. |
| UID and GID | Map the one authorized guest UID and GID to the host user. `chown` to the mapped identity succeeds when Linux permits. Other IDs are stored only if guest-only metadata policy is enabled, otherwise return `EPERM`. Never attempt host ownership escalation. |
| Atime, mtime, ctime, birth time | Return nanosecond values at the precision the host actually supports. Map explicit `utimensat`, `UTIME_NOW`, and `UTIME_OMIT`. Let host reads update atime according to the mounted volume, unless `noatime` is configured. ctime is broker-derived from host metadata changes and is never directly settable. |
| Extended attributes | Map safe `user.*` names through a reversible namespace. Keep `com.apple.*` hidden unless allowlisted. Store Linux-only values in the durable metadata database keyed by object ID. Enforce size limits and create/replace flags. Quarantine and provenance xattrs are not silently copied into executable guest policy namespaces. |
| POSIX ACLs | Translate simple ACLs to mode bits. Persist nontrivial `system.posix_acl_access` and default ACLs in guest metadata, enforce them in the daemon, and update mode-mask interactions exactly. If enforcement is not complete, do not advertise ACL support. |
| Host ACLs | Host ACL denial remains an upper bound. The broker does not grant access that the launching host user lacks and does not rewrite host ACLs merely to satisfy a guest request. |
| File flags | Map immutable and append-only only when host flags and permissions provide equivalent enforcement. Store benign unsupported flags as guest metadata if exact. Reject flags whose enforcement cannot be guaranteed. |
| `stat`, inode, link count | Stable guest inode per object generation. Report logical link count without broker pins, mapped ownership, actual size and block allocation, and host filesystem limits. Detect inode reuse with birth time and generation state. |
| `statfs` and limits | Report the backing volume's space and name limits, adjusted for protocol bounds. Do not report guest-local overlay space for host-backed paths. |
| Case and Unicode | Expose the backing volume's case behavior. Preserve names accepted by macOS. Reject byte sequences that cannot be represented rather than normalize them to a different name. Test case-insensitive APFS collisions and Unicode normalization aliases. |
| Special files | Regular files, directories, and symlinks are required. Device nodes are denied. Unix sockets, FIFOs, and broker-unsafe ioctls return `EOPNOTSUPP` and should live in a guest-local runtime directory. |
| Clone and copy offload | Use `clonefileat` or host copy offload only when source and destination are inside the attachment and semantics match. Otherwise fall back to bounded read/write in the guest or return `EOPNOTSUPP` as the opcode requires. |
| `syncfs` and detach | Drain prior requests, sync all dirty guest pages, sync touched host objects and metadata WAL, then acknowledge. Detach cannot complete successfully while an error remains unreported. |

### Correctness invariants

1. An acknowledged namespace mutation corresponds to one committed host namespace result or one recoverable journal transaction.
2. A guest handle never retargets because a pathname was deleted and reused.
3. No success reply precedes the host syscall whose visibility it promises.
4. `fsync` success means all earlier writes for that handle have reached the requested host durability boundary.
5. No request can follow a symlink or rename race outside the immutable project root.
6. Filtering is evaluated on every component before the host syscall, not only during `readdir`.
7. Guest-only metadata is committed atomically with the corresponding visible operation, or recovery finishes or rolls it back before reattach.

## Watch delivery without per-file host descriptors

### Guest-originated operations

Linux VFS generates ordinary inotify and fanotify events for successful syscalls made through the FUSE mount. The guest daemon also sends a mutation sequence to the host. The broker tags its resulting metadata update so the later FSEvents echo is recognized. Echo suppression favors duplicates over omission because duplicate/coalesced notifications are legal and a missed change is worse.

### Host-originated operations

The host uses one recursive FSEvents stream for the attached root with file events enabled. FSEvents does not require one open descriptor per watched directory. Start the stream before the initial scan. Maintain file-ID and parent snapshots for visible paths, skipping filtered subtrees. For each event batch:

1. Reject paths outside the canonical root and filtered paths.
2. Restat the affected path and parent through contained operations.
3. Compare the snapshot to derive create, delete, content, metadata, and probable rename changes.
4. Send inode and dentry invalidations to `cloisterfs` before the conservative fsnotify event.
5. Coalesce repeated content changes for a short bounded window, while preserving namespace ordering.

When FSEvents reports `MustScanSubDirs`, `KernelDropped`, or `UserDropped`, rescan the affected visible subtree, invalidate all uncertain dentries and inodes, and inject `IN_Q_OVERFLOW` and `FAN_Q_OVERFLOW` so consumers know to rescan. Do not pretend to reconstruct intermediate operations that FSEvents already coalesced.

### Required Linux FUSE extension

Stock FUSE unsolicited messages include inode, entry, and delete invalidation, but no arbitrary fsnotify injection. Cache invalidation by itself does not produce all inotify events. Full mode therefore adds a narrow `FUSE_NOTIFY_FS_EVENT` ABI to the Cloister guest kernel. Its payload identifies already-known parent and child FUSE node IDs, name, mask, and rename cookie. The kernel validates that node IDs belong to the same FUSE connection, invalidates caches, then calls the normal fsnotify helpers. Rename pairs use one cookie. Overflow requires no path.

The patch is guest-only and distributed in the controlled Cloister VM image. It must be small enough to rebase and test against every supported kernel. It must not accept arbitrary host inode numbers or paths. The daemon can only name FUSE nodes belonging to its own connection.

For guest `fanotify` permission events, guest-originated opens and writes can be mediated normally. A host-originated write has already happened and can only generate a notification event, never a meaningful pre-content permission decision. Capability reporting must state this distinction.

Stock-kernel compatibility mode uses FUSE invalidations and a documented conservative `IN_ATTRIB` rescan nudge only if integration tests prove the target watcher reacts. It is not equivalent and is not the default for Tier 3 general availability.

## Consistency and caching

The default is strict writethrough:

- Do not negotiate FUSE writeback cache.
- Prefer direct I/O with `FUSE_DIRECT_IO_ALLOW_MMAP` on a kernel that passes the mapping suite.
- If direct-I/O mmap is unavailable, use page cache with `entry_timeout=0`, `negative_timeout=0`, a very short attribute timeout, `FOPEN_KEEP_CACHE` disabled, and immediate host-event invalidation.
- Preserve read-ahead and parallel reads, but never cache namespace truth in the broker beyond generation-checked metadata.
- A successful ordinary write means the host syscall completed, not that storage is power-loss durable. `fsync`, `fdatasync`, `msync`, and `syncfs` define durability boundaries as on a local POSIX filesystem.
- A host edit becomes visible after the FSEvents and invalidation latency. The service publishes observed p50, p95, and p99 invalidation delay.

An optional relaxed cache mode may be considered later for read-mostly repositories. It cannot become default until editor-save, mmap, and concurrent host-edit tests pass. Revisit this only after real traces show the strict mode is too slow.

## Filtering and guest-local overlays

Filter rules are attachment policy, compiled on the host and repeated in the guest capability. Rules match path components, never untrusted glob expansion. The hardened preset includes:

```text
**/node_modules      guest-local, persistent by project ID and VM
**/.git              guest-local, persistent by project ID and VM
**/{build,dist,out,target,.next,.cache}  guest-local, policy-selected persistence
```

The host broker returns no host lookup, content, metadata, or event for a filtered prefix. The guest daemon diverts that subtree into `/var/lib/cloisterfs/overlays/<project-id>/...` on the VM disk. From the application perspective it remains under `~/workspace`. The broker therefore never walks or opens the host version, even when a guest requests it explicitly.

`node_modules` and build directories are straightforward and should normally be ephemeral or cache-managed. `.git` is not straightforward. Masking it means guest and host Git metadata are not live mirrors. The safe design is:

1. On first attach, the host control plane creates a Git bundle and index description without exposing raw `.git` through the filesystem broker.
2. The guest initializes persistent local Git metadata against the shared working tree.
3. Guest commits remain in the guest-local repository during the session.
4. On detach, new objects and refs are exported into host-side `refs/cloister/<profile>/<session>/...` through a semantic Git bridge. The bridge never moves the host's current branch or overwrites its index unless the original HEAD and index preconditions still match and the user opted in.
5. Any unimported guest refs block destructive cleanup and are reported with a recovery command.

This is a deliberate semantic boundary. A profile that requires host and guest Git metadata to be identical should use a policy that exposes `.git`, accepting its metadata traffic, or should not claim that `.git` is filtered. The CLI must display the selected behavior. Silent divergence is unacceptable.

Filters apply only to the project broker. SSH, GPG, Downloads, Claude extensions, agent data, and Ollama models remain separate policy resources. They must never be folded into the project capability.

## Descriptor-safety proof and resource bounds

Let:

- `A` be active project attachments.
- `W` be the global bounded broker worker count, initially 64.
- `K` be the small constant of host descriptors required by one operation, at most 6 for root, source parent, destination parent, operand, journal, and temporary output.
- `C` be constant daemon descriptors for the control socket, listener, logs, journal, and FSEvents implementation.

Then the broker host descriptor bound is:

```text
FD_broker <= C + 2A + K*W
```

With `A=2`, `W=64`, and `K=6`, the operation-dependent portion is at most 388 descriptors plus the small daemon constant. It does not contain project file count `N`, guest inode count `I`, guest open handle count `H`, directory count `D`, or guest watch count `Q`.

Why:

- A guest `OPEN` creates a broker handle record and a hardlink pin, not a retained host descriptor.
- Each `READ`, `WRITE`, `GETATTR`, `FSYNC`, or namespace operation opens descriptors, completes one bounded operation, and closes them in a `defer` or equivalent unconditional cleanup path.
- `READDIRPLUS` holds one directory descriptor only while producing one bounded response.
- FSEvents uses one root stream, not one kqueue descriptor per file or directory.
- Metadata snapshots, inode tables, pins, and watch state consume memory or filesystem entries, not host descriptors.
- The guest's `/dev/fuse`, vsock, application files, and local overlay descriptors exist in the guest kernel and do not consume the macOS system-wide file table.

Enforcement is stronger than the proof alone:

- A semaphore prevents more than `W` descriptor-using operations.
- Startup sets a conservative `RLIMIT_NOFILE` for the broker.
- A descriptor census samples `/dev/fd` and exports `host_fd_current`, `host_fd_peak`, `ops_inflight`, `guest_handles`, `pin_count`, and `visible_paths`.
- A test traverses millions of entries and opens hundreds of thousands of guest handles while asserting the host descriptor plateau.
- The process self-quarantines an attachment before reaching its descriptor budget and never retries `EMFILE` or `ENFILE` in a loop.

Supplemental virtiofs mounts remain outside this bound. Their trees must stay small and are measured separately. A future broad supplemental mount would recreate the original risk and must be rejected by policy.

## Crash, restart, and detach safety

### Journal model

Use a checksummed write-ahead log plus compacted state. Every non-idempotent request has attachment epoch, monotonically increasing request sequence, and random operation ID. The broker records intent before a multi-step emulation and records commit before replying. Replayed completed IDs return the recorded result. Incomplete transactions are inspected and completed or rolled back based on host identities, never blindly repeated.

The journal covers:

- Object ID to host identity and current names.
- Open-handle references and pin paths.
- Unnamed temporary objects.
- Guest-only metadata.
- Pending namespace transactions.
- Last delivered host event generation and scan checkpoint.
- Detach state and unimported guest-local Git refs.

### Failure behavior

| Failure | Required result |
|---|---|
| Broker process crash | Guest requests block for a bounded reconnect period. The restarted broker authenticates the same epoch, rebuilds handles from durable pins, replays idempotent results, invalidates all caches, and resumes. If recovery cannot prove identity, the mount becomes read-only or returns `EIO`; it never guesses. |
| Guest daemon crash | Linux FUSE mount fails requests until systemd restarts the daemon or the mount is replaced. Broker retains journal and pins during a lease timeout. Reconnect validates epoch and open-handle table. |
| VM graceful stop | Cloister drains and detaches before `Backend.Stop`. `syncfs` errors abort a clean stop unless the user explicitly forces it. |
| VM hard stop | Host retains pins and dirty attachment state. The next attach runs recovery before allowing writes. Ordinary writes not followed by a durability call have normal crash-loss exposure. Acknowledged `fsync` data must survive. |
| Host sleep or transport loss | Requests pause, then fail with `EHOSTDOWN` or `EIO` after timeout. Writes are never queued indefinitely without an application-visible result. Reconnect invalidates caches. |
| FSEvents overflow | Invalidate the affected subtree, perform a generation scan, and inject watcher overflow. Do not continue with a falsely precise event stream. |
| Forced detach | Drain what can be drained, record dirty state, revoke the token, and preserve pins and Git exports until recovery. Report that processes received I/O errors. |

Periodic garbage collection removes a private pin only after its journal says no live or recoverable handle references it. Age alone is not proof. Project deletion or movement on the host never causes the collector to traverse or delete outside the private state directory.

## Security review

### Boundary preservation

The Tier 3 boundary is narrower than today's workspace mount. One attachment capability names one canonical project. The guest sees no sibling project, ancestor, host home, or absolute host path. Supplemental shares do not share the broker token or namespace.

SSH and GPG remain distinct read-only virtiofs mounts, with the existing guest remount enforcement. Headless Claude extension directories remain read-only as today. The broker design does not change tunnel consent or make host services reachable beyond existing loopback and profile policy.

### Threats and controls

| Threat | Control |
|---|---|
| Guest sends `../`, absolute paths, malformed Unicode, or NULs | Protocol accepts length-delimited relative components, rejects forbidden bytes and absolute roots, and resolves with beneath-root syscalls. |
| Symlink swap between check and use | No check followed by unrestricted path use. Resolve and operate relative to held root and parent descriptors with Darwin resolve-beneath flags. |
| Host renames project root during attachment | Root identity is immutable for the epoch. Each operation verifies it. Mismatch freezes writes and requires reattach. |
| Guest guesses another attachment | Random 256-bit capability bound to VM identity, profile, project ID, epoch, and relay peer. Tokens expire and are single attachment use. |
| Compromised guest floods operations | Per-attachment request, byte, memory, path-depth, and outstanding-I/O quotas. Global worker pool and fair scheduling prevent starvation. |
| Filter bypass through hardlink or symlink | Apply filter decisions after canonical component resolution and to both link endpoints. Do not expose object IDs from filtered paths. |
| Access to private pin state | State is outside the exported namespace, mode `0700`, random-named, and reachable only by host-relative broker syscalls. Guest object IDs cannot address its paths. |
| Device-node or socket abuse | Reject device nodes and unsupported special types. Direct local runtime sockets and FIFOs to VM-local storage. |
| UID or ACL escalation | Host-user access is the upper bound. Guest ownership mapping cannot cause host `chown` or ACL grants beyond that user. |
| Protocol parser exploit | Fixed upper bounds, fuzzed decoder, no recursive allocation from wire lengths, version negotiation, and process sandboxing. The broker runs as the user with a minimal launchd sandbox profile. |
| Stolen host control socket | Socket mode `0600`, peer UID check, no path-bearing attach from untrusted callers, and route authorization in the CLI. |

Security tests must race symlink replacement, directory rename, hardlink creation, filter transitions, and project-root movement while operations run. A string-prefix containment test is never sufficient.

## Performance expectations and engineering choices

The broker will not beat virtiofs on unfiltered metadata microbenchmarks. Each cold operation adds guest FUSE dispatch, a vsock round trip, secure host path resolution, and a host open and close. The value is a bounded host resource model and a much smaller visible tree.

Mitigations that preserve descriptor safety:

- Negotiate 1 MiB or larger bounded read and write requests, scatter-gather where the FUSE library supports it, and multiple concurrent requests.
- Use a fixed worker pool and per-inode ordering only for mutations that need it. Independent reads and stats run concurrently.
- Use `READDIRPLUS` and macOS bulk directory metadata APIs for one response at a time, then close the directory descriptor.
- Cache immutable protocol metadata and stable object IDs in memory, but validate host generations according to strict cache policy.
- Coalesce invalidations and watcher events over a small time window without reordering namespace transactions.
- Keep build products, dependency trees, package caches, sockets, and temporary outputs on the guest disk through filters. This removes the highest-I/O trees from the round trip entirely.
- Give interactive requests priority over background snapshot reconciliation during watcher storms.

Benchmark against current Colima 0.10.3 and Lima 2.2.0 on the same project and hardware. Required measurements include cold and warm `git status` with exposed and local `.git`, editor save latency, recursive stat, 1 GiB sequential read and write, 4 KiB random I/O, package install into local `node_modules`, incremental build, full build, 10,000-event watcher burst, host-event propagation latency, CPU, memory, and macOS open files.

Do not set a made-up throughput promise. Set release thresholds after the prototype baseline. A reasonable product gate is that source editing and incremental build remain interactive, filtered full builds are dominated by guest-local I/O, and host descriptor use stays within the proven ceiling. Publish regressions relative to virtiofs rather than hiding them.

## Observability and operations

`cloister doctor` and `cloister status` should report:

- Workspace mode, project ID, guest mount path, attachment state, session references, and age.
- Broker and guest protocol versions and negotiated capabilities.
- Strict or relaxed cache mode.
- Full or degraded host-watch mode.
- Current and peak host descriptor count, operation queue depth, FSEvents lag, invalidation latency, and last overflow.
- Pin count, incomplete transactions, dirty detach state, and local overlay disk use.
- Filter rules and `.git` mode.

Logs use project ID and relative paths only at debug level. Default logs must not record personal absolute host paths. Metrics must avoid filenames. A support bundle redacts canonical roots and tokens.

## Honest risk register

| Risk | Severity and failure mode | Mitigation and release gate |
|---|---|---|
| Unlink-while-open without retained fds | Critical. A path-only handle can retarget or lose an unlinked inode. | Same-volume hardlink pin, identity verification, durable handle journal. Refuse volumes without reliable hardlinks. Stress unlink, rename, replace, and restart. |
| Host-originated watch semantics | Critical. Stock FUSE invalidation does not generate the complete inotify stream, and FSEvents can coalesce or drop events. Dev servers may silently miss changes. | Guest kernel fsnotify extension, stream-before-scan, snapshot reconciliation, overflow injection, real watcher test suite. Degraded mode is labeled and not GA. |
| `MAP_SHARED` coherence and dirty invalidation | Critical. A host edit can race dirty guest pages and silently lose one writer. | Writethrough, direct-I/O mmap capability gate, ordered invalidation, conflict detection, `msync` tests, documented data-race boundary. Disable writable shared mmap if the kernel and library combination fails. |
| Broker crash between multi-step emulation calls | Critical. O_TMPFILE, pins, or metadata can leak or become visible incorrectly. | Checksummed WAL, idempotency keys, same-volume state machine, fail-stop recovery tests at every injected crash point. |
| Symlink and rename escape race | Critical security boundary failure. | Kernel-enforced resolve-beneath operations, immutable root identity, adversarial race harness, fail closed on unsupported hosts. |
| `.git` filtering divergence | High. Guest commits or index changes may appear lost on the host. | Persistent local repo, semantic bundle import to quarantine refs, preconditioned optional integration, explicit UI, never auto-overwrite host refs or index. |
| macOS and Linux metadata mismatch | High. ACL, xattr, case, Unicode, flags, and timestamp behavior can silently differ. | Capability negotiation, durable guest metadata, explicit unsupported errors, filesystem-specific matrix on case-sensitive and case-insensitive APFS. |
| Directory handles after external host removal | High. No legal fd-free directory pin exists. | Stable object identity, return `ESTALE`, never retarget. Document network-filesystem behavior and test editor recovery. |
| Advisory locks versus host processes | High for databases and some build tools. | Broker lock manager for guest clients, explicit capability warning for cross-host locks, recommend VM-local storage for lock-sensitive databases. Do not claim host lock coherence. |
| Lume parity | High. Lume currently passes only the first share and uses a macOS guest, so the Linux FUSE implementation does not apply. | Reject `workspace_mode: broker` on Lume until a vsock relay and FSKit or equivalent guest adapter pass the same suite. No silent Tier 2 fallback. |
| FUSE library gaps | High. A library may compile while omitting TMPFILE, rename flags, mmap, or notify behavior. | Opcode audit and wire-level conformance tests before selection. Keep protocol semantics independent of the library. |
| Host volume variation | High. SMB, exFAT, external APFS, and network filesystems have different hardlink, sync, flag, and case behavior. | Per-attachment capability probe. Start read-only or fail. Never infer from filesystem name alone. |
| FSEvents storm or overflow | Medium to high. Cache can remain stale or CPU can spike. | Bounded coalescing, fair queues, scan budget, overflow events, full reconciliation, and lag health state. |
| Performance below virtiofs | Medium. Metadata-heavy unfiltered workloads can feel slow. | Filter hot trees, bulk readdir, parallelism, large I/O, measure before defaulting Tier 3. Keep Tier 2 as an explicit option during rollout. |
| Pin link-count visibility on host | Medium. Host tools may observe an extra hardlink. | Subtract pins in guest stats, use a private state directory, document host visibility, test backup and IDE behavior. There is no honest fd-free way to hide both the fd and the extra link. |
| Detach while background tools remain | Medium. Forced unmount can cause errors or data loss. | Session cgroups, reference counts, normal unmount gate, PID reporting, dirty-state recovery, no successful lazy detach. |
| Supplemental virtiofs regrowth | Medium. A newly broad supplemental mount can recreate fd exhaustion. | Size and path policy, descriptor telemetry by process, code review gate that project paths never enter `[]Mount` in broker mode. |

## Phased AI-assisted implementation plan

The estimates are agent build and validation cycles, not human-weeks. They assume access to a real Apple Silicon Mac for integration tests and permit parallel test generation only after the protocol contract is stable.

### Phase 0: feasibility spikes, 2 to 4 elapsed hours, 1 to 2 agent cycles

- Prove a guest FUSE request can traverse a Lima VZ vsock relay to a host Unix socket.
- Prove `O_RESOLVE_BENEATH`, `renameatx_np` flags, hardlink pins, `F_FULLFSYNC`, and FSEvents behavior on supported macOS versions.
- Audit candidate low-level FUSE libraries for protocol 7.37 and 7.39 behavior.
- Prototype the guest kernel fsnotify notification with one create, modify, rename pair, delete, and overflow event.

Exit gate: kill or revise the design if vsock relay ownership, direct-I/O mmap, or safe event injection cannot be demonstrated. This phase is genuinely hard because it validates platform seams, not application plumbing.

### Phase 1: read-only vertical slice, 4 to 8 elapsed hours, 2 to 3 agent cycles

- Define the versioned protocol and capability handshake.
- Implement attach authorization, contained lookup, getattr, readlink, open-record, read, readdirplus, release, and detach.
- Mount one project at `~/workspace` through Colima with all project virtiofs mounts absent.
- Add fd census and a million-entry traversal test.

Most protocol scaffolding is mechanical. Secure path resolution and identity reuse tests require careful review.

### Phase 2: mutation and durability core, 12 to 24 elapsed hours, 4 to 6 agent cycles

- Implement create, write, truncate, mkdir, unlink, hardlink, symlink, rename, exchange, no-replace, pins, O_TMPFILE, fsync, fdatasync, fsyncdir, syncfs, and recovery WAL.
- Add guest metadata for UID, GID, ACL, and xattrs.
- Add byte-range and flock manager.
- Build fault injection at every journal transition.

This is the hardest filesystem phase. Atomicity, restart idempotence, and open-handle identity need model-based tests, not only examples.

### Phase 3: mmap, cache, and watches, 8 to 16 elapsed hours, 3 to 5 agent cycles

- Validate direct-I/O shared mmap and fallback behavior against the supported guest kernel.
- Implement FSEvents snapshot reconciliation and invalidations.
- Land and package the minimal guest FUSE fsnotify extension.
- Exercise real Node, Go, Rust, Python, Cursor, and editor watchers under host and guest edits.

This phase is also genuinely hard. Event behavior can look correct in a demo while losing events under coalescing or overflow.

### Phase 4: filters and local storage, 4 to 8 elapsed hours, 2 to 3 agent cycles

- Add component filter compiler and guest-local path router.
- Add persistent and ephemeral overlay lifecycles and quota reporting.
- Implement the `.git` bundle bridge, quarantine refs, conflict detection, and recovery UX.

Local routing is mechanical. Safe `.git` handoff is not and should ship behind its own feature flag.

### Phase 5: Cloister lifecycle integration, 4 to 8 elapsed hours, 2 to 3 agent cycles

- Add config migration, `StartOptions`, broker transport capabilities, and supplemental-only mount construction.
- Integrate create, enter, add-stack, rebuild, repair, reset, resize, snapshot, agent, and stop flows.
- Update guest config, bashrc, container binds, status, doctor, and launch adapters.
- Add session reference counting, systemd scopes, and clean detach.

This phase is mostly mechanical once lifecycle state transitions are specified, but it has a broad regression surface.

### Phase 6: hardening and performance, 12 to 24 elapsed hours, 4 to 6 agent cycles

- Fuzz protocol and path resolver, race containment, run crash matrix, and validate volume capabilities.
- Benchmark against virtiofs, tune request size and concurrency, and run watcher storms.
- Add upgrade, downgrade, broker restart, VM hard-stop, and stale-state recovery tests.
- Conduct a focused security review and ship opt-in to one real project.

Expected total: roughly 18 to 28 focused agent cycles and 2 to 5 days of elapsed build and hardware-test time, assuming Phase 0 succeeds. AI can generate protocol handlers, fixtures, fault matrices, and integration wiring quickly. It cannot remove the need for real-kernel, real-FSEvents, real-editor, and power-loss-adjacent validation.

### Phase 7: Lume parity, separately gated

- Add a Lume-owned VZ vsock relay.
- Implement a macOS guest filesystem adapter using a supported FSKit path if it can meet writable semantics, or another explicitly reviewed mechanism.
- Replace Linux inotify tests with macOS FSEvents and editor watcher tests inside the guest.
- Pass the same atomicity, durability, path containment, filter, descriptor, and crash suite.

Do not estimate this as mechanical reuse. The guest VFS and event APIs are different. Until this phase passes, Tier 3 is Colima-only and Lume users receive a clear unsupported message.

## Test strategy

### Test layers

1. Pure model tests compare broker state-machine results with a reference POSIX model.
2. Host syscall adapter tests run on temporary APFS volumes, including case-sensitive and case-insensitive variants.
3. Protocol conformance tests replay every opcode, malformed frame, timeout, duplicate, and reconnect sequence.
4. Linux guest tests compare the broker mount with ext4 for supported operations.
5. Differential tests run the same randomized operation trace on ext4 and the broker, then compare namespace, content, metadata, errors, and event stream modulo documented remote-event coalescing.
6. Fault injection terminates broker, guest daemon, relay, VM, and transport at every mutation journal point.
7. Security race tests continuously swap symlinks and rename parents while reads and writes attempt escape.
8. Real application tests use editors, language servers, package managers, build tools, Git, Claude, Cursor, and Codex.

### Correctness matrix

| Area | Required cases |
|---|---|
| Atomic saves | Vim swap and rename, VS Code and Cursor save, JetBrains safe write pattern, temp in target directory, rename over open target, fsync before rename, crash before and after each step. |
| Rename | File and nonempty directory, same and different parents, overwrite, open source, open target, no-replace race, exchange race, host concurrent rename, watcher rename cookies. |
| Temporary files | O_TMPFILE create, write, link, unlink before link, multiple links, final close, broker crash at every state, unsupported-volume rejection. |
| Links | Hardlink content identity, link counts with pins, symlink inside root, absolute escape, `..` escape, symlink loops, link swap race, cross-volume error. |
| Open lifetime | Unlink while read-open and write-open, rename while open, replace path with new inode, broker restart, VM hard stop, last-release cleanup. |
| Data I/O | Short read and write, partial error, concurrent writes, O_APPEND from many guest processes plus host appenders, O_SYNC and O_DSYNC, direct I/O alignment errors. |
| Mapping | MAP_PRIVATE, MAP_SHARED read and write, msync async and sync, truncate under mapping, host edit under clean mapping, host edit under dirty mapping, unmap and fsync error propagation. |
| Sparse and size | Hole creation, seek data and hole, extend truncate, shrink truncate, punch hole if advertised, clone, copy range, block count. |
| Metadata | chmod and umask, mapped chown, rejected chown, setuid clearing, nanosecond times, atime policy, ctime changes, xattr CRUD and flags, POSIX ACL inheritance, host ACL denial, immutable and append flags. |
| Names | Long names and paths, invalid UTF-8 rejection, composed and decomposed Unicode, case-only rename, case collision, reserved broker name, filtered component at every depth. |
| Directories | Stable readdir cookies, concurrent insert and delete, external rename, open removed directory returns ESTALE, large readdirplus, filtered names absent. |
| Watches | Guest create, write, attrib, close-write, move pair, delete, delete-self, host equivalents, rapid replacement, subtree move, 10,000-event burst, FSEvents overflow, broker reconnect, no event from filtered host subtree. |
| Locks | Shared and exclusive flock, byte ranges, blocking wakeup, deadlock and interruption, owner close, daemon reconnect, documented unrelated-host limitation. |
| Durability | fsync and fdatasync error injection, syncfs, graceful detach, forced detach, broker crash before and after ack, journal corruption, pin recovery, full-sync capability. |
| Security | Traversal strings, symlink race, root rename, hardlink to outside attempt, token replay, wrong VM, malformed lengths, operation flood, descriptor ceiling. |
| Filters | Local node_modules install, full build output, persistent overlay reattach, ephemeral cleanup, quota exhaustion, `.git` seed, commit export, conflicting host HEAD and index, recovery of unimported refs. |

### Descriptor regression test

Create a synthetic project with at least one million visible files and filtered trees larger than the visible tree. Traverse and stat it repeatedly, create 200,000 guest open handles, install watches recursively, and run a second VM concurrently. Sample the broker and all Virtualization.framework processes. Broker host descriptor count must plateau at the configured formula and remain independent of file, handle, and watch counts. Supplemental virtiofs counts must remain small and separately attributable.

### Real single-project end-to-end gate

Use one representative production-sized project:

1. Configure a Colima profile with `workspace_mode: broker` and the hardened filter preset.
2. Open the project from a host file through Claude, Cursor, and Codex launch routes.
3. Verify all land at the same stable guest path and share one attachment lease.
4. Edit on host and guest while language servers and dev servers watch. Verify bounded propagation and no missed final state.
5. Install dependencies and build. Confirm `node_modules` and outputs live only on the VM disk and host filtered trees are never opened by the broker.
6. Exercise Git under the selected `.git` policy and recover guest commits on the host through quarantine refs.
7. Kill and restart `cloister-fsd`, then hard-stop and restart the VM during controlled writes. Verify recovery and explicit errors.
8. Close the last routed app. Confirm processes drain, sync succeeds, `~/workspace` unmounts, FSEvents stops, capability revokes, and broker descriptors return to idle baseline.
9. Start another profile on another project and repeat concurrently while observing `kern.maxfiles` and process descriptor counts.

## Rollout and acceptance

Ship in this order:

1. Developer-only feature flag, read-only.
2. Writable opt-in on local APFS with filters excluding `.git` bridge automation.
3. Full fsnotify guest image and crash-tested durability.
4. Opt-in beta for one project per Colima profile.
5. Default for newly created Colima profiles only after descriptor, correctness, and performance gates hold.
6. Existing profiles migrate only with explicit consent and a reversible config change.
7. Lume remains unsupported until Phase 7 parity.

General availability blockers:

- Any path escape or ambiguous root identity.
- Host descriptors growing with tree size, guest open handles, or watches.
- Silent editor-save, mmap, append, rename, or durability mismatch.
- Missed final-state host changes without a watcher overflow or rescan signal.
- Unrecoverable guest commit caused by `.git` filtering.
- Broker restart that can retarget an open handle.
- Silent fallback to a broad writable mount.

## Alternatives rejected

### Custom virtiofsd-compatible host server

The protocol can express much of the required surface, but Apple's VZ API configures directory shares inside the VM-owning process and does not offer Cloister a stable external virtiofsd socket contract. Replacing that implementation couples Tier 3 to Lima or Lume internals, does not solve FUSE's remote fsnotify gap, and makes on-demand attachment harder. It is worth revisiting only if Apple or Lima exposes a supported external daemon endpoint.

### macFUSE or userspace NFSv4 host mount, then re-export

This creates two filesystem translations and two cache coherency domains. NFS server-side changes still do not naturally become exact guest inotify events. macFUSE may also need privileged installation on the trusted host. The extra layer has no security or descriptor advantage over operating directly beneath a contained broker root.

### File sync instead of a live filesystem

Sync avoids remote syscall impedance but cannot meet immediate writable live-view, atomic rename, open-unlink, mmap, or deterministic detach requirements without becoming a distributed conflict-resolution system. It is a different product tier.

## Decisions to revisit

- After strict-mode traces, decide whether relaxed caching is valuable enough to support.
- After the `.git` bridge beta, decide whether default filtering should hide all `.git`, only high-churn object data, or leave Git metadata host-backed.
- If upstream Linux accepts a general FUSE fsnotify notification, drop the Cloister guest patch.
- If Lima exposes a supported external filesystem device or generic vsock relay, remove the private integration.
- If Apple FSKit supports a complete writable network-style guest adapter with invalidation hooks, prioritize Lume parity.

## Technical references

- Linux FUSE UAPI opcodes and notifications, including `FUSE_TMPFILE`, `FUSE_RENAME2`, `FUSE_DIRECT_IO_ALLOW_MMAP`, and the existing invalidation notification set: <https://github.com/torvalds/linux/blob/master/include/uapi/linux/fuse.h>
- Linux FUSE cache timeout and invalidation implementation: <https://github.com/torvalds/linux/blob/master/fs/fuse/dir.c>
- Linux FUSE mapped-write, release, flush, and fsync ordering: <https://github.com/torvalds/linux/blob/master/fs/fuse/file.c>
- Linux fsnotify hooks used for create, delete, rename, modify, open, and close events: <https://github.com/torvalds/linux/blob/master/include/linux/fsnotify.h>
- Apple FSEvents guidance on stream lifecycle, coalesced changes, dropped events, and scan-before-reconcile ordering: <https://developer.apple.com/library/archive/documentation/Darwin/Conceptual/FSEvents_ProgGuide/UsingtheFSEventsFramework/UsingtheFSEventsFramework.html>
- Apple virtio-vsock connection and file descriptor interface: <https://developer.apple.com/documentation/virtualization/vzvirtiosocketconnection>
- Apple Virtualization framework directory sharing model: <https://developer.apple.com/documentation/virtualization/vzvirtiofilesystemdeviceconfiguration>
- Apple FSKit user-space filesystem framework for a possible future Lume guest adapter: <https://developer.apple.com/documentation/fskit>
- Lima default mount and experimental `mountInotify` configuration: <https://github.com/lima-vm/lima/blob/master/templates/default.yaml>
- Lima's current mount-inotify implementation history, which forwards host notifications and nudges guest timestamps rather than extending FUSE fsnotify: <https://github.com/lima-vm/lima/pull/1913>
