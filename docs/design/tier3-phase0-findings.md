# Tier 3 Phase 0 feasibility spike findings

Date: 2026-08-20

Scope: platform kill-gates only. This spike did not build a writable filesystem and did not change production Cloister code.

## Recommendation

**NO-GO for proceeding to Tier 3 Phase 1.**

The containment and watch gates failed. The FUSE stack tested on the actual guest also failed the full capability gate. Transport and pinning produced useful positive evidence, but did not satisfy their complete flip criteria. The verdict document says any one Phase 0 flip is sufficient to stop Tier 3. Three gates are currently failed.

| Gate | Result | Decisive evidence |
|---|---|---|
| 1. Transport and identity | **UNKNOWN** | A supported SSH reverse Unix-socket relay worked for 100 fresh data connections with application authentication and deduplication. The actual VZ vsock path failed and fell back to usernet, Lima exposes no public general-purpose vsock relay, and the required one-hour run was not performed. |
| 2. macOS containment primitives | **FAIL** | Darwin resolve-beneath flags protected `openat`, `unlinkat`, and `renameatx_np`, but flagless `mkdirat` and `symlinkat` followed an intermediate symlink and created objects outside the root. No already-audited fd-walk alternative exists. |
| 3. FUSE capability | **FAIL** | The kernel offered protocol 7.39 and `DIRECT_IO_ALLOW_MMAP`. The tested current go-fuse library negotiated only 7.28. The guest's installed libfuse 3.14 is capped at 7.38. No tested stack negotiated all required features, and no cached shared-mmap fallback has passed differential tests. |
| 4. Watch feasibility | **FAIL** | Accepted FUSE inode and dentry invalidations changed the data returned by a later read, but a real Go fsnotify watcher received 0 events. Stock FUSE invalidation alone cannot drive the required rebuild signal. |
| 5. Hardlink pin feasibility | **UNKNOWN** | 100,000 cycles had zero retargets, premature deletions, identity mismatches, or leaked pins, with flat process fd use. Every link and source unlink changed ctime, and crash recovery, FSEvents feedback, and target-tool effects remain untested. |

## Environment and safety

- Host: Apple Silicon macOS 26.5.2, Darwin 25.5.0, APFS worktree volume.
- Colima: 0.10.3.
- Lima: 2.2.0.
- Actual running guest: Ubuntu 24.04.4 LTS, kernel `6.8.0-100-generic`, aarch64.
- Guest systemd: 255.4.
- Guest FUSE devices and configuration: `/dev/fuse` present, `/dev/vsock` present, `CONFIG_FUSE_FS=y`, `CONFIG_VIRTIO_VSOCKETS=m`.
- Initial host fd headroom: `kern.num_files=24,554`, `kern.maxfiles=491,520`, 5.0 percent used.
- Final cleanup check: `kern.num_files=24,589`, `kern.maxfiles=491,520`, 5.0 percent used. The 35-fd host-wide change is background activity, not retained probe fds.
- No VM was started by this spike. Tests used an already-running Colima profile. The spike did not add any host tree mount.
- Every probe process, SSH tunnel, guest FUSE mount, guest temporary binary, and Unix socket created by the spike was stopped or removed after its test.

Probe source is under `spike/tier3-phase0/`. Generated binaries and runtime directories are intentionally not retained.

## Gate 1: transport and identity

### Exact criterion

From `fs-broker-verdict.md`, Phase 0 flip gate 1:

> Transport and identity, flip: sustain one hour under load and 100 forced reconnects with zero duplicated committed mutation IDs, zero cross-profile attachment, and 100 percent rejection of expired, replayed, wrong-profile, and wrong-epoch tokens. If the actual Colima relay cannot be owned and restarted without patching an unstable private hypervisor interface, flip.

The cross-family addition repeats the same threshold and says not to flip if the Colima relay cannot be owned and restarted without an unstable private-interface patch.

### What Lima 2.2.0 actually exposes

Lima 2.2.0 creates a VZ virtio socket device, but its VZ integration exposes no public arbitrary-port host listener or general relay configuration:

- [`startVsockForwarder`](https://github.com/lima-vm/lima/blob/v2.2.0/pkg/driver/vz/vsock_forwarder.go) is an unexported driver method. It listens on host TCP and calls another unexported method, `SocketDevices().Connect(port)`, in the host-to-guest direction.
- The only call in [`vm_darwin.go`](https://github.com/lima-vm/lima/blob/v2.2.0/pkg/driver/vz/vm_darwin.go) is hard-coded to guest port 22 for SSH.
- [`GuestAgentConn`](https://github.com/lima-vm/lima/blob/v2.2.0/pkg/driver/vz/vz_driver_darwin.go) similarly connects from host to the guest-agent vsock port.
- `limactl tunnel --help` says only SOCKS tunnels are implemented.
- The supported [`portForwards`](https://github.com/lima-vm/lima/blob/v2.2.0/templates/default.yaml) schema does support Unix sockets and `reverse: true`. That mechanism is SSH forwarding, not a public raw VZ vsock listener.

On the actual profile, Lima logged that SSH on the vsock port failed and that it was using the usernet forwarder. The profile had systemd 255, while the Lima log specifically suggested that systemd older than 256 may prevent this path. Thus even Lima's hard-coded SSH-over-vsock optimization was not active for the tested Colima image.

A direct broker vsock listener would require changing or extending Lima's private VZ driver today. A supported SSH reverse Unix-socket relay does not require such a patch, so it was tested as the fallback relay allowed by the criterion.

### Relay and identity probe

`relay_probe.py` ran a mode-0600 host Unix server. A separately owned SSH reverse forward exposed a mode-0600 guest Unix socket. The application protocol used profile, epoch, expiration, one-time nonce, HMAC token, and mutation ID fields.

Measured result:

```text
host socket mode: 0600, owned by the invoking host user
guest socket mode: 0600, owned by the invoking guest user
fresh data connections: 100
elapsed: 0.032226 seconds
rate: 3,103.1 connections/second
committed mutation IDs: 1
deduplicated repeats: 99
expired token: rejected
replayed token: rejected
wrong profile: rejected
wrong epoch: rejected
```

The 100 connections each closed and reconnected to the relayed Unix socket. This proves functional reconnect and application-layer identity over Lima's supported SSH path. It does not prove 100 host relay process restarts, one hour under load, or direct VZ vsock ownership.

### Result: UNKNOWN

The public SSH relay is ownable, reconnectable, authenticated, and does not need an unstable hypervisor patch. The preferred direct VZ path is not exposed and did not operate on the actual image. Because the one-hour requirement and repeated relay-process restart behavior were not tested, this gate is not a PASS. If direct raw vsock, rather than the supported SSH relay, is mandatory for the design, this result becomes a FAIL.

## Gate 2: macOS containment primitives

### Exact criterion

From `fs-broker-verdict.md`, Phase 0 flip gate 2:

> Containment primitives, flip: the supported macOS and target volume must provide root-relative, no-follow operations for all required single-path and dual-path mutations, or an audited fd-walk alternative must already exist. Run at least 1,000,000 concurrent traversal, symlink-swap, parent-rename, and root-replacement attempts with zero syscalls reaching an outside sentinel. Any escape or ambiguous root identity flips immediately.

### SDK surface

macOS has no Linux `openat2(2)` API and no Linux `RESOLVE_BENEATH` contract. The installed macOS SDK nevertheless has Darwin-specific containment flags that the original Tier 3 blueprint understated:

```text
O_NOFOLLOW_ANY            0x20000000
O_RESOLVE_BENEATH         0x00001000
AT_RESOLVE_BENEATH        0x00002000
RENAME_NOFOLLOW_ANY       0x00000010
RENAME_RESOLVE_BENEATH    0x00000020
RENAME_SWAP               0x00000002
```

It also exposes `freadlink(int fd, ...)`, available from macOS 13, so a symlink can be opened as a symlink with `O_SYMLINK | O_NOFOLLOW_ANY` and its raw link text read from the fd.

The important limitation is uneven syscall coverage:

- `openat` accepts `O_NOFOLLOW_ANY` and `O_RESOLVE_BENEATH`.
- `unlinkat` accepted `AT_RESOLVE_BENEATH` in the probe. `linkat` also has a flags argument, but its complete containment behavior was not tested.
- `renameatx_np` accepts `RENAME_SWAP`, `RENAME_NOFOLLOW_ANY`, and `RENAME_RESOLVE_BENEATH` for a dual-path atomic operation.
- `mkdirat`, `mknodat`, and `symlinkat` have no flags argument. They cannot directly request no-follow-any or resolve-beneath behavior.

### APFS probe

`darwin_containment_probe.c` used a private root and outside sentinel on the worktree APFS volume.

Positive results:

```text
open through escaping symlink with O_RESOLVE_BENEATH: rejected, ENOTCAPABLE
open through escaping symlink with O_NOFOLLOW_ANY: rejected, ELOOP
open of an inside path with both flags: succeeded
freadlink of symlink fd: returned ../outside
renameatx_np RENAME_SWAP inside root: succeeded
renameatx_np escape with resolve-beneath: rejected, ENOTCAPABLE
unlinkat escape with AT_RESOLVE_BENEATH: rejected, outside sentinel remained
existing root fd after pathname replacement: still opened the original inside object
```

The concurrent symlink-swap loop performed 1,000,000 `openat` attempts using `O_RESOLVE_BENEATH`:

```text
inside opens: 24,918
outside opens: 0
rejected: 967,012
other transient EINVAL results: 8,070
```

Decisive negative results:

```text
mkdirat(rootfd, "escape/new", ...): succeeded outside the root
symlinkat(..., rootfd, "escape/new"): succeeded outside the root
```

Both calls followed an intermediate symlink because those APIs have no containment flags. The probe removed the outside test objects immediately.

An fd walk could open each parent with `O_DIRECTORY | O_NOFOLLOW_ANY | O_RESOLVE_BENEATH`, then issue the flagless mutation against the parent fd. That is a new security-critical resolver, not an already-audited alternative. It must also define what happens when a host process renames the opened parent outside the authorized root between validation and mutation.

The million-attempt test covered traversal and symlink swapping plus a separate root-path replacement check. It did not exhaust the verdict's complete parent-rename and root-identity race matrix for every mutation opcode. That missing stress coverage cannot rescue the API-coverage failure.

### Result: FAIL

The supported SDK does not provide direct root-relative, no-follow containment for all required mutations, and an audited fd-walk alternative does not already exist. Two required flagless mutation families were demonstrated escaping through an intermediate symlink. This meets the criterion's immediate flip condition.

## Gate 3: FUSE capability

### Exact criterion

From `fs-broker-verdict.md`, Phase 0 flip gate 3:

> FUSE capability, flip for the full design: the target guest kernel and chosen library must negotiate and expose the needed opcodes and invalidations without silently degrading them. Missing `TMPFILE` may be an explicit scoped limitation only if every target editor and tool passes with `EOPNOTSUPP`. Missing both direct-I/O shared mmap and a differential-tested cached fallback flips.

### Actual kernel and negotiation

The actual Colima guest ran kernel `6.8.0-100-generic`. Its FUSE INIT request was captured from a real mount:

```text
kernel INIT: 7.39
kernel offered: DIRECT_IO_ALLOW_MMAP
tested go-fuse response: 7.28
tested go-fuse accepted flags: ASYNC_READ, BIG_WRITES, AUTO_INVAL_DATA,
READDIRPLUS, NO_OPEN_SUPPORT, PARALLEL_DIROPS, MAX_PAGES, INIT_EXT
```

Linux 6.8's [FUSE UAPI](https://github.com/torvalds/linux/blob/v6.8/include/uapi/linux/fuse.h) defines:

- `RENAME2`, protocol 7.23.
- `SYNCFS`, protocol 7.34.
- `TMPFILE`, protocol 7.37.
- `DIRECT_IO_ALLOW_MMAP`, protocol 7.39.
- Inode, entry, and delete notification opcodes.

The kernel side therefore reaches the required protocol level and advertises the direct-I/O mmap bit.

The library side did not pass:

- go-fuse v2.11.0 is a mature low-level Go library and knows opcode constants for `RENAME2`, `SYNCFS`, and `TMPFILE`, plus the mmap capability bit. Its Linux implementation still sets `_OUR_MINOR_VERSION = 28`, so the real mount negotiated 7.28 and cannot legitimately expose the newer operations.
- The guest-installed libfuse 3.14.0 uses a protocol 7.38 header. It covers `RENAME2`, `SYNCFS`, `TMPFILE`, and invalidation APIs, but predates 7.39 and cannot negotiate `DIRECT_IO_ALLOW_MMAP`.
- Current upstream [libfuse 3.18.2](https://github.com/libfuse/libfuse/releases/tag/fuse-3.18.2) has newer protocol headers and the direct-I/O mmap capability. It was not built, packaged, or negotiated in this guest during the spike.

No cached shared-mmap fallback has a differential test result. No real `O_TMPFILE`, `RENAME_EXCHANGE`, `syncfs`, or writable `MAP_SHARED` behavior suite was run through a candidate daemon.

### Result: FAIL

The actual kernel is capable, but neither low-level userspace stack present or tested negotiated the full required feature set. The explicit full-design criterion therefore fails today because the tested stack lacks direct-I/O shared mmap and there is no tested cached fallback. This gate may be reopened by packaging current libfuse and passing wire-level and behavioral tests on the actual guest, but source-level availability is not a Phase 0 PASS.

## Gate 4: watch feasibility

### Exact criterion

From `fs-broker-verdict.md`, Phase 0 flip gate 4:

> Watch feasibility, flip: for 100 repetitions of host create, atomic replace, rename, delete, subtree move, and a 10,000-event burst, each representative Node watcher, Go watcher, and editor must either observe a usable event and the correct final state, or receive overflow and demonstrably rescan. One silent missed final-state rebuild after the broker reports healthy flips. Exact host syscall reconstruction is not required because FSEvents cannot promise it.

### Real FUSE invalidation and fsnotify probe

`fuse-invalidation/main.go` mounted a one-file FUSE filesystem in the actual guest. `fsnotify/main.go`, using fsnotify v1.10.1, watched both the mounted directory and file.

The sequence was:

1. Watcher read `watched.txt` as `before`.
2. The daemon changed its backing data to `after` without a guest VFS mutation.
3. The daemon sent `NOTIFY_INVAL_INODE` and `NOTIFY_INVAL_ENTRY`.
4. The kernel returned success for both notifications.
5. The watcher waited four seconds, then explicitly reread the file.

Measured result:

```text
NOTIFY_INVAL_INODE result: success
NOTIFY_INVAL_ENTRY result: success
Go fsnotify events: 0
explicit final read: after
```

This is exactly the failure mode at issue. Invalidation made a later read coherent, but it did not create an inotify event that would cause a dev server to perform that read or rebuild.

`FUSE_NOTIFY_DELETE` has a limited delete-specific inotify effect in current implementations. It does not provide general create, modify, atomic replace, rename-pair, subtree-move, or overflow injection. Current upstream Linux FUSE UAPI has invalidation, delete, resend, epoch, and prune notifications, but no general `FUSE_NOTIFY_FS_EVENT` opcode.

The full 100-repetition Node, Go, editor, and 10,000-event matrix was not run because the single healthy invalidation with zero Go events already meets the criterion's one-silent-miss flip condition.

### Kernel-patch maintainability

Full fidelity requires the proposed custom guest-kernel notification patch or an equivalent upstream ABI. That creates a continuing maintenance obligation:

- Rebase and review the patch for every supported guest kernel security update.
- Build and distribute a custom kernel and modules rather than consuming the Lima image kernel unchanged.
- Pin or continuously qualify Colima and Lima guest-image updates, because an image update can replace the kernel independently of Cloister.
- Retest inotify and fanotify masks, rename cookies, overflow, inode lifetime, FUSE connection identity, and cache ordering after each rebase.
- Maintain a degraded stock-kernel mode separately, because users who boot an unpatched kernel cannot silently receive full-correctness claims.

This is high and ongoing maintenance, not a one-time Phase 0 patch. The actual guest already illustrates update coupling with its distribution-specific `6.8.0-100-generic` kernel.

### Result: FAIL

FUSE invalidation alone did not drive a real Linux watcher. A custom guest-kernel notification patch is required for the full design, and the unpatched stack produced the exact silent missed-rebuild condition in the flip criterion.

## Gate 5: hardlink pin feasibility

### Exact criterion

From `fs-broker-verdict.md`, Phase 0 flip gate 5:

> Pin feasibility, flip: 100,000 open, rename, replace, unlink, reconnect, and close cycles must produce zero handle retargets, zero premature deletion, and zero leaked pins after recovery. Persistent self-generated FSEvents feedback, unbounded ctime churn that breaks target tools, or inability to create safe same-volume state on the supported project volume flips.

### APFS pin probe

`pin_probe.c` used a same-volume hardlink as the handle pin. Each cycle:

1. Created and opened a uniquely identified live file.
2. Recorded its device, inode, ctime, and content.
3. Created a hardlink pin and closed the original fd.
4. Renamed the live path, placed a replacement at the original path, then unlinked the renamed source.
5. Reopened the pin as a reconnect surrogate and verified original identity and content.
6. Closed and removed the pin, then checked for leakage.

Measured result for 100,000 cycles:

```text
elapsed: 81.812 seconds
rate: 1,222 cycles/second
handle retargets: 0
premature deletions: 0
leaked pins: 0
device or inode identity mismatches: 0
ctime changed on pin link: 100,000 of 100,000
ctime changed on source unlink: 100,000 of 100,000
first link ctime delta: 366,128 ns
first source-unlink ctime delta: 778,089 ns
process fd count: 3 before, 3 peak, 3 after
system kern.num_files: 24,400 before, 24,374 after, delta -26
pin present after test: no
```

The system fd delta is background noise on a live host. The useful fd result is that the probe process did not grow above its baseline of 3 while the number of objects and cycles grew.

The pin preserved bytes and inode identity through the tested namespace loop. It also perturbed source ctime on every pin creation and on every unlink of the remaining project name. A production design that pins on every guest `OPEN` therefore changes host-visible ctime for read-only opens and transient handles. The probe did not establish whether Git, editors, indexers, backup tools, or build systems tolerate that behavior.

This loop did not kill and restart a broker with a durable journal, inject crashes between journal transitions, measure FSEvents feedback, test pin-directory recovery, or exercise non-APFS project volumes. Reopening the pin is a reconnect surrogate, not recovery proof.

### Result: UNKNOWN

The exact 100,000-cycle identity and leakage counts passed at small scale, and host fd use remained bounded. The mandatory ctime side effect occurred on every cycle, while recovery and FSEvents behavior remain untested. That is positive feasibility evidence, but not enough for a PASS and not enough to show the criterion's target-tool ctime condition is safe.

## Real-hardware validation still required

This spike ran on the target Apple Silicon host and an actual Colima guest. The following complete gates still require dedicated validation:

1. Transport: a clean no-mount Colima profile with the intended guest image, a supported owned relay process, one hour of load, 100 relay-process failures and reconnects, VM reboot, broker restart, and all authentication negative cases. If raw vsock remains required, Lima needs a supported general relay interface before testing can pass.
2. Containment: every path-taking opcode on every supported macOS version and project volume, including parent rename and root replacement races. An fd-walk design needs a separate security audit and at least the full 1,000,000-attempt matrix before this gate can be reconsidered.
3. FUSE: current libfuse packaged into the guest, actual 7.39 or newer negotiation, `TMPFILE`, `RENAME2` flags, `SYNCFS`, invalidation, and a differential writable shared-mmap suite. Test both direct I/O and any claimed cached fallback.
4. Watches: Node chokidar, Go fsnotify, and representative editors for all required operation classes and the 10,000-event burst. Full mode must use the actual patched kernel image and demonstrate overflow-triggered rescans.
5. Pins: journaled broker crash and restart at every transition, FSEvents feedback and echo suppression, real Git and editor workloads, backup and indexer interaction, safe private state on each supported volume, and cleanup after VM loss.

## Final decision

Do not proceed to the Tier 3 read-only slice or any writable filesystem implementation from this Phase 0 result. Preserve the shared mount and lifecycle seam work, and follow the verdict's Tier 2 fallback path if product work continues.

Visible summary:

```text
1. Transport and identity: UNKNOWN
2. macOS containment primitives: FAIL
3. FUSE capability: FAIL
4. Watch feasibility: FAIL
5. Hardlink pin feasibility: UNKNOWN

Recommendation: NO-GO
```
