package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: fsnotify-probe MOUNTPOINT")
		os.Exit(2)
	}
	path := filepath.Join(os.Args[1], "watched.txt")
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		panic(err)
	}
	defer watcher.Close()
	if err := watcher.Add(os.Args[1]); err != nil {
		panic(err)
	}
	if err := watcher.Add(path); err != nil {
		panic(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
	fmt.Printf("WATCH_READY before=%q\n", before)
	deadline := time.After(4 * time.Second)
	events := 0
	for {
		select {
		case event := <-watcher.Events:
			events++
			fmt.Printf("EVENT %s\n", event)
		case err := <-watcher.Errors:
			fmt.Printf("WATCH_ERROR %v\n", err)
		case <-deadline:
			after, err := os.ReadFile(path)
			if err != nil {
				panic(err)
			}
			fmt.Printf("WATCH_DONE events=%d after=%q\n", events, after)
			return
		}
	}
}
