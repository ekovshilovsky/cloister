package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type probeRoot struct {
	fs.Inode
	file *probeFile
}

type probeFile struct {
	fs.Inode
	mu   sync.RWMutex
	data []byte
}

func (r *probeRoot) OnAdd(ctx context.Context) {
	r.file = &probeFile{data: []byte("before\n")}
	child := r.NewPersistentInode(ctx, r.file, fs.StableAttr{Mode: syscall.S_IFREG, Ino: 2})
	r.AddChild("watched.txt", child, false)
}

func (f *probeFile) Getattr(_ context.Context, _ fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out.Mode = 0o644
	out.Ino = 2
	out.Size = uint64(len(f.data))
	return 0
}

func (f *probeFile) Open(_ context.Context, _ uint32) (fs.FileHandle, uint32, syscall.Errno) {
	return nil, fuse.FOPEN_KEEP_CACHE, 0
}

func (f *probeFile) Read(_ context.Context, _ fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if off >= int64(len(f.data)) {
		return fuse.ReadResultData(nil), 0
	}
	end := int(off) + len(dest)
	if end > len(f.data) {
		end = len(f.data)
	}
	return fuse.ReadResultData(f.data[off:end]), 0
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: fuse-invalidation-probe MOUNTPOINT")
		os.Exit(2)
	}
	root := &probeRoot{}
	server, err := fs.Mount(os.Args[1], root, &fs.Options{
		MountOptions: fuse.MountOptions{
			Debug:       true,
			DirectMount: true,
			FsName:      "tier3-invalidation-probe",
			Name:        "tier3probe",
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "mount: %v\n", err)
		os.Exit(1)
	}
	if err := server.WaitMount(); err != nil {
		fmt.Fprintf(os.Stderr, "wait mount: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("READY")

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGUSR1, syscall.SIGTERM, syscall.SIGINT)
	for {
		sig := <-signals
		if sig != syscall.SIGUSR1 {
			break
		}
		root.file.mu.Lock()
		root.file.data = []byte("after\n")
		root.file.mu.Unlock()
		contentErr := root.file.EmbeddedInode().NotifyContent(0, 0)
		entryErr := root.EmbeddedInode().NotifyEntry("watched.txt")
		fmt.Printf("INVALIDATED content_err=%v entry_err=%v\n", contentErr, entryErr)
	}
	if err := server.Unmount(); err != nil {
		fmt.Fprintf(os.Stderr, "unmount: %v\n", err)
		os.Exit(1)
	}
}
