package agentgrid

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestParseEmptyIsEmptyList(t *testing.T) {
	for _, input := range []string{"", "   ", "\n\t"} {
		entries, err := Parse([]byte(input))
		if err != nil {
			t.Fatalf("Parse(%q) error: %v", input, err)
		}
		if len(entries) != 0 {
			t.Fatalf("Parse(%q) = %#v, want empty", input, entries)
		}
	}
}

func TestParseRejectsMalformedSoCloisterNeverClobbersUnknownState(t *testing.T) {
	if _, err := Parse([]byte("{not json")); err == nil {
		t.Fatal("Parse accepted malformed JSON")
	}
	if _, err := Parse([]byte(`{"path":"x"}`)); err == nil {
		t.Fatal("Parse accepted a non-array list")
	}
}

func TestParseDropsEntriesWithoutPath(t *testing.T) {
	entries, err := Parse([]byte(`[{"path":"/a","name":"a","addedAt":"t"},{"name":"nopath","addedAt":"t"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != "/a" {
		t.Fatalf("entries = %#v", entries)
	}
}

func TestMarshalIsDeterministicSortedWithTrailingNewline(t *testing.T) {
	data, err := Marshal([]Entry{
		{Path: "/home/u/workspaces/b", Name: "b", AddedAt: "t2"},
		{Path: "/home/u/workspaces/a", Name: "a", AddedAt: "t1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if data[len(data)-1] != '\n' {
		t.Fatal("Marshal output missing trailing newline")
	}
	var round []Entry
	if err := json.Unmarshal(data, &round); err != nil {
		t.Fatalf("Marshal output is not valid JSON: %v", err)
	}
	if round[0].Path != "/home/u/workspaces/a" || round[1].Path != "/home/u/workspaces/b" {
		t.Fatalf("Marshal did not sort by path: %#v", round)
	}
}

func TestMarshalEmptyListIsJSONArrayNotNull(t *testing.T) {
	data, err := Marshal(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != "[]\n" {
		t.Fatalf("Marshal(nil) = %q, want %q", got, "[]\n")
	}
}

func TestUpsertAddsNewAndPreservesManualEntries(t *testing.T) {
	existing := []Entry{{Path: "/manual", Name: "manual", AddedAt: "orig"}}
	desired := []Entry{{Path: "/proj", Name: "proj", AddedAt: "new"}}
	merged := Upsert(existing, desired)
	want := []Entry{
		{Path: "/manual", Name: "manual", AddedAt: "orig"},
		{Path: "/proj", Name: "proj", AddedAt: "new"},
	}
	if !reflect.DeepEqual(merged, want) {
		t.Fatalf("Upsert = %#v, want %#v", merged, want)
	}
}

func TestUpsertKeepsOriginalAddedAtButUpdatesName(t *testing.T) {
	existing := []Entry{{Path: "/proj", Name: "old-name", AddedAt: "first"}}
	desired := []Entry{{Path: "/proj", Name: "new-name", AddedAt: "second"}}
	merged := Upsert(existing, desired)
	if len(merged) != 1 {
		t.Fatalf("Upsert duplicated a path: %#v", merged)
	}
	if merged[0].Name != "new-name" || merged[0].AddedAt != "first" {
		t.Fatalf("Upsert = %#v, want name updated and addedAt preserved", merged[0])
	}
}

func TestRemovePathsDropsOnlyNamedAndIgnoresUnknown(t *testing.T) {
	existing := []Entry{
		{Path: "/a", Name: "a", AddedAt: "t"},
		{Path: "/b", Name: "b", AddedAt: "t"},
	}
	kept := RemovePaths(existing, []string{"/a", "/missing"})
	if len(kept) != 1 || kept[0].Path != "/b" {
		t.Fatalf("RemovePaths = %#v, want only /b", kept)
	}
}

func TestNewEntryUsesRFC3339UTC(t *testing.T) {
	entry := NewEntry("/p", "p", time.Date(2026, 8, 31, 16, 30, 0, 0, time.FixedZone("x", 3600)))
	if entry.AddedAt != "2026-08-31T15:30:00Z" {
		t.Fatalf("AddedAt = %q, want UTC RFC3339", entry.AddedAt)
	}
}
