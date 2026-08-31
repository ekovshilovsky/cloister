// Package agentgrid manages the on-disk list of project paths that the Agent
// Grid daemon will serve to a paired client. The daemon reads this list from a
// file in its data directory and re-reads it per request, so cloister grants or
// revokes access by rewriting the file. This package owns only cloister's own
// add and remove decisions and the file encoding; the SSH transport that reads
// and writes the file inside the VM lives in the command layer.
package agentgrid

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// SharedListRelPath is the list file location relative to the guest home
// directory, matching the daemon data directory cloister configures for the
// agentgrid stack.
const SharedListRelPath = ".agent-grid-daemon/shared-projects.json"

// Entry is one shared project path with a display name and the time it was
// added. The field names are the integration contract with the daemon.
type Entry struct {
	Path    string `json:"path"`
	Name    string `json:"name"`
	AddedAt string `json:"addedAt"`
}

// Parse decodes the list file. Empty or whitespace-only input is a valid empty
// list. Malformed or non-array input is rejected so cloister never silently
// discards a list it could not read and then overwrites it. Entries without a
// path are dropped.
func Parse(data []byte) ([]Entry, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, nil
	}
	var entries []Entry
	if err := json.Unmarshal(trimmed, &entries); err != nil {
		return nil, fmt.Errorf("parsing shared project list: %w", err)
	}
	cleaned := make([]Entry, 0, len(entries))
	for _, entry := range entries {
		if strings.TrimSpace(entry.Path) == "" {
			continue
		}
		cleaned = append(cleaned, entry)
	}
	return cleaned, nil
}

// Marshal encodes entries as a two-space-indented JSON array with a trailing
// newline. Entries are sorted by path so a rewrite is deterministic regardless
// of decision order. An empty list encodes as [] rather than null.
func Marshal(entries []Entry) ([]byte, error) {
	sorted := append([]Entry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	if sorted == nil {
		sorted = []Entry{}
	}
	data, err := json.MarshalIndent(sorted, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding shared project list: %w", err)
	}
	return append(data, '\n'), nil
}

// Upsert merges desired entries into existing ones, keyed by path. An existing
// path keeps its original AddedAt but adopts the desired name. New paths are
// appended with their provided AddedAt. Entries cloister does not name are left
// untouched. The result is sorted by path.
func Upsert(existing, desired []Entry) []Entry {
	byPath := make(map[string]Entry, len(existing))
	for _, entry := range existing {
		byPath[entry.Path] = entry
	}
	for _, entry := range desired {
		if current, ok := byPath[entry.Path]; ok {
			current.Name = entry.Name
			byPath[entry.Path] = current
			continue
		}
		byPath[entry.Path] = entry
	}
	merged := make([]Entry, 0, len(byPath))
	for _, entry := range byPath {
		merged = append(merged, entry)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].Path < merged[j].Path })
	return merged
}

// RemovePaths returns existing entries whose path is not in paths. Unknown
// paths are ignored. The result is sorted by path.
func RemovePaths(existing []Entry, paths []string) []Entry {
	drop := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		drop[path] = struct{}{}
	}
	kept := make([]Entry, 0, len(existing))
	for _, entry := range existing {
		if _, remove := drop[entry.Path]; remove {
			continue
		}
		kept = append(kept, entry)
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].Path < kept[j].Path })
	return kept
}

// NewEntry builds an entry stamped with the given time in RFC3339 UTC.
func NewEntry(path, name string, now time.Time) Entry {
	return Entry{Path: path, Name: name, AddedAt: now.UTC().Format(time.RFC3339)}
}
