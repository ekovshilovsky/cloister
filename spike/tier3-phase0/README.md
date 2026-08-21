# Tier 3 Phase 0 throwaway probes

These probes are feasibility evidence only. They are not production filesystem code.

- `darwin_containment_probe.c` exercises the current macOS SDK and APFS behavior for root-relative open, no-follow, resolve-beneath, symlink reads, dual-path rename, swap rename, root replacement, and a 1,000,000-attempt symlink-swap race.
- `pin_probe.c` exercises a same-volume hardlink pin through open, rename, replace, unlink, reopen as a reconnect surrogate, and close. It records inode identity, ctime changes, leaked pins, and fd counts.
- `fuse-invalidation/main.go` mounts a one-file FUSE filesystem and changes its backing content before sending only inode and dentry invalidations.
- `fsnotify/main.go` watches that mount with Go fsnotify and reports whether invalidation alone produced a Linux watcher event.
- `relay_probe.py` tests a supported Lima reverse Unix-socket relay with 100 fresh data connections, mutation deduplication, and rejection of expired, replayed, wrong-profile, and wrong-epoch tokens. It does not turn Lima's private VZ driver methods into a general vsock API.

Build and run from the worktree root:

```sh
xcrun clang -O2 -Wall -Wextra -pthread spike/tier3-phase0/darwin_containment_probe.c -o spike/tier3-phase0/darwin_containment_probe
xcrun clang -O2 -Wall -Wextra spike/tier3-phase0/pin_probe.c -o spike/tier3-phase0/pin_probe
./spike/tier3-phase0/darwin_containment_probe
./spike/tier3-phase0/pin_probe 100000
```

The Go probes are cross-compiled for the actual Linux guest during the spike. Run the daemon as root with a private guest mountpoint, start the watcher, send `SIGUSR1` to the daemon, and then send `SIGTERM` so it unmounts.
