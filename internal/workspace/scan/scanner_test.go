package scan

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestScanClassifierMatrixAndSafeManifestInventory(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	writeTestFile(t, filepath.Join(project, "src/main.go"), "package main")
	writeTestFile(t, filepath.Join(project, ".env.local"), "DO_NOT_REPORT=this-value")
	writeTestFile(t, filepath.Join(project, "config.local.json"), `{"token":"DO_NOT_REPORT"}`)
	writeTestFile(t, filepath.Join(project, "appsettings.Local.json"), `{"token":"DO_NOT_REPORT"}`)
	writeTestFile(t, filepath.Join(project, "client.pem"), "DO_NOT_REPORT")
	writeTestFile(t, filepath.Join(project, "data.sqlite"), "DO_NOT_REPORT")
	writeTestFile(t, filepath.Join(project, "backup.sql"), "DO_NOT_REPORT")
	writeTestFile(t, filepath.Join(project, "dist/output.js"), "generated")
	writeTestFile(t, filepath.Join(project, ".agent/instructions.md"), "review me")
	writeTestFile(t, filepath.Join(project, ".agent/accounts.json"), "DO_NOT_REPORT")
	writeTestFile(t, filepath.Join(project, ".mcp.json"), "DO_NOT_REPORT")
	writeTestFile(t, filepath.Join(project, "large.bin"), strings.Repeat("x", 20))
	writeTestFile(t, filepath.Join(project, "internal/build/source.go"), "x")
	writeTestFile(t, filepath.Join(project, "migrations/0001_init.sql"), "create table sample (id int);")
	writeTestFile(t, filepath.Join(project, "package.json"), `{"engines":{"node":">=22"},"scripts":{"test":"vitest --reporter DO_NOT_REPORT","build":"tsc -p ."}}`)
	writeTestFile(t, filepath.Join(project, "pyproject.toml"), "[project]\nname = \"sample\"\n")
	writeTestFile(t, filepath.Join(project, "go.mod"), "module example.invalid/app\ngo 1.26\n")
	writeTestFile(t, filepath.Join(project, "global.json"), `{"sdk":{"version":"10.0.100"}}`)
	writeTestFile(t, filepath.Join(project, "compose.yaml"), "services:\n  db:\n    image: local\n  api:\n    build: .\n")
	writeTestFile(t, filepath.Join(project, "node_modules/pkg/index.js"), "must be pruned")
	writeTestFile(t, filepath.Join(project, "node_modules_backup/index.js"), "x")
	writeTestFile(t, filepath.Join(project, "vendor/library.txt"), "x")
	writeTestFile(t, filepath.Join(project, ".git/objects/one"), "must be pruned")

	opened := []string{}
	proposal, err := Scan(Options{
		SourceRoot:     root,
		Projects:       []ProjectDescriptor{{ID: "project", Path: "project", Kind: ProjectShared}},
		Generator:      "test",
		CreatedAt:      time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		LargeFileBytes: 10,
		OpenFile: func(path string) (io.ReadCloser, error) {
			opened = append(opened, filepath.Base(path))
			return os.Open(path)
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	wantOpened := []string{"compose.yaml", "global.json", "go.mod", "package.json"}
	if !reflect.DeepEqual(opened, wantOpened) {
		t.Fatalf("opened files = %v, want only safe manifests %v", opened, wantOpened)
	}
	assertFindingClass(t, proposal.Findings, ".env.local", ClassSecretLocalConfig, DecisionReview)
	assertFindingClass(t, proposal.Findings, "config.local.json", ClassSecretLocalConfig, DecisionReview)
	assertFindingClass(t, proposal.Findings, "appsettings.Local.json", ClassSecretLocalConfig, DecisionReview)
	assertFindingClass(t, proposal.Findings, "client.pem", ClassSecretLocalConfig, DecisionExclude)
	assertFindingClass(t, proposal.Findings, "data.sqlite", ClassDatabase, DecisionExclude)
	assertFindingClass(t, proposal.Findings, "backup.sql", ClassDatabaseDump, DecisionExclude)
	assertFindingClass(t, proposal.Findings, "dist", ClassGeneratedArtifact, DecisionExclude)
	assertFindingClass(t, proposal.Findings, ".agent/instructions.md", ClassAgentConfig, DecisionReview)
	assertFindingClass(t, proposal.Findings, ".agent/accounts.json", ClassHostPrivateAgentState, DecisionExclude)
	assertFindingClass(t, proposal.Findings, ".mcp.json", ClassHostPrivateAgentState, DecisionExclude)
	assertFindingClass(t, proposal.Findings, "large.bin", ClassUnknownLarge, DecisionReview)
	assertFindingClass(t, proposal.Findings, "package.json", ClassApplicationManifest, DecisionInclude)
	assertFindingClass(t, proposal.Findings, "pyproject.toml", ClassApplicationManifest, DecisionInclude)
	assertFindingClass(t, proposal.Findings, "compose.yaml", ClassServiceManifest, DecisionInclude)
	assertFindingClass(t, proposal.Findings, "node_modules", ClassDependency, DecisionExclude)
	assertFindingClass(t, proposal.Findings, ".git", ClassDependency, DecisionExclude)
	assertFindingClass(t, proposal.Findings, "node_modules_backup/index.js", ClassSource, DecisionInclude)
	assertFindingClass(t, proposal.Findings, "vendor/library.txt", ClassSource, DecisionInclude)
	assertFindingClass(t, proposal.Findings, "internal/build/source.go", ClassSource, DecisionInclude)
	assertFindingClass(t, proposal.Findings, "migrations/0001_init.sql", ClassDatabaseScript, DecisionInclude)

	if got := runtimeNames(proposal.Runtimes); !reflect.DeepEqual(got, []string{"dotnet", "go", "node"}) {
		t.Fatalf("runtimes = %v", got)
	}
	if got := commandNames(proposal.Commands); !reflect.DeepEqual(got, []string{"build", "test"}) {
		t.Fatalf("commands = %v", got)
	}
	for _, command := range proposal.Commands {
		if command.Path != "package.json" {
			t.Fatalf("command %q evidence path = %q, want package.json", command.Name, command.Path)
		}
	}
	if got := serviceNames(proposal.Services); !reflect.DeepEqual(got, []string{"api", "db"}) {
		t.Fatalf("services = %v", got)
	}

	data, err := MarshalProposal(*proposal)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "DO_NOT_REPORT") || strings.Contains(string(data), root) {
		t.Fatalf("proposal leaked content or absolute source path: %s", data)
	}
}

func TestScanNeverOpensSecretLikeFiles(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{
		".env", "credentials.json", "client.pem", "appsettings.Local.json",
		"database.db", "dump.sql", "migrations/0001_init.sql", "reports/monthly.sql",
		".env.example", ".env.sample", ".env.template", ".env.example.backup",
		"appsettings.Local.example.json",
	} {
		writeTestFile(t, filepath.Join(root, "project", name), "trap-secret")
	}
	_, err := Scan(Options{
		SourceRoot: root,
		Projects:   []ProjectDescriptor{{ID: "project", Path: "project", Kind: ProjectLocal}},
		OpenFile: func(path string) (io.ReadCloser, error) {
			return nil, errors.New("unexpected open: " + filepath.Base(path))
		},
	})
	if err != nil {
		t.Fatalf("secret-like file was opened: %v", err)
	}
}

func TestScanTreatsDirenvConfigurationAsMetadataOnlyAndPrunesGeneratedState(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	for _, name := range []string{
		".envrc", ".envrc.local", ".envrc.example", ".direnvrc",
		"envrc.go", "internal/envrc/loader.go",
		".direnv/bin/tool", ".direnv/package.json", ".direnv/compose.yaml",
	} {
		writeTestFile(t, filepath.Join(project, filepath.FromSlash(name)), "DO_NOT_REPORT")
	}

	var opened []string
	proposal, err := Scan(Options{
		SourceRoot: root,
		Projects:   []ProjectDescriptor{{ID: "project", Path: "project", Kind: ProjectShared}},
		OpenFile: func(path string) (io.ReadCloser, error) {
			opened = append(opened, filepath.Base(path))
			return nil, errors.New("unexpected open")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(opened) != 0 {
		t.Fatalf("direnv paths reached the injected opener: %v", opened)
	}
	assertFindingClass(t, proposal.Findings, ".envrc", ClassSecretLocalConfig, DecisionReview)
	assertFindingClass(t, proposal.Findings, ".envrc.local", ClassSecretLocalConfig, DecisionReview)
	assertFindingClass(t, proposal.Findings, ".direnvrc", ClassSecretLocalConfig, DecisionReview)
	assertFindingClass(t, proposal.Findings, ".envrc.example", ClassSource, DecisionInclude)
	assertFindingClass(t, proposal.Findings, "envrc.go", ClassSource, DecisionInclude)
	assertFindingClass(t, proposal.Findings, "internal/envrc/loader.go", ClassSource, DecisionInclude)
	assertFindingClass(t, proposal.Findings, ".direnv", ClassGeneratedArtifact, DecisionExclude)
	for _, finding := range proposal.Findings {
		if strings.HasPrefix(finding.Path, ".direnv/") {
			t.Fatalf("scanner descended into generated direnv state: %#v", finding)
		}
	}
	if !containsValue(proposal.Policy.PrunePatterns, ".direnv") {
		t.Fatalf("prune patterns %v do not document .direnv", proposal.Policy.PrunePatterns)
	}
}

func TestScanPrunesKnownDotNetConfigurationSubtreesButKeepsOtherBinSource(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	writeTestFile(t, filepath.Join(project, "bin", "tools", "source.go"), "package tools")
	writeTestFile(t, filepath.Join(project, "obj", "source", "source.go"), "package source")
	for _, path := range []string{
		"bin/Debug/generated.json",
		"bin/Release/generated.json",
		"obj/Debug/generated.json",
		"obj/Release/generated.json",
		"src/BIN/dEbUg/generated.json",
	} {
		writeTestFile(t, filepath.Join(project, filepath.FromSlash(path)), "{}")
	}

	proposal, err := Scan(Options{
		SourceRoot: root,
		Projects:   []ProjectDescriptor{{ID: "project", Path: "project", Kind: ProjectLocal}},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertFindingClass(t, proposal.Findings, "bin/tools/source.go", ClassSource, DecisionInclude)
	assertFindingClass(t, proposal.Findings, "obj/source/source.go", ClassSource, DecisionInclude)
	for _, directory := range []string{"bin/Debug", "bin/Release", "obj/Debug", "obj/Release", "src/BIN/dEbUg"} {
		assertFindingClass(t, proposal.Findings, directory, ClassGeneratedArtifact, DecisionExclude)
	}
	for _, finding := range proposal.Findings {
		if strings.HasSuffix(finding.Path, "generated.json") {
			t.Fatalf("scanner descended into generated .NET subtree: %#v", finding)
		}
	}
}

func TestScanPrunesPrivateDirectoriesBeforeOpeningAllowlistedBasenames(t *testing.T) {
	root := t.TempDir()
	traps := []string{
		".ssh/package.json",
		".aws/go.mod",
		".gnupg/compose.yaml",
		".cursor/projects/package.json",
	}
	for _, name := range traps {
		writeTestFile(t, filepath.Join(root, "project", filepath.FromSlash(name)), "trap-secret")
	}
	var opened []string
	proposal, err := Scan(Options{
		SourceRoot: root,
		Projects:   []ProjectDescriptor{{ID: "project", Path: "project", Kind: ProjectLocal}},
		OpenFile: func(path string) (io.ReadCloser, error) {
			opened = append(opened, path)
			return nil, errors.New("unexpected open")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(opened) != 0 {
		t.Fatalf("private allowlisted basenames were opened: %v", opened)
	}
	for _, directory := range []string{".ssh", ".aws", ".gnupg", ".cursor/projects"} {
		assertFindingClass(t, proposal.Findings, directory, map[string]FindingClass{
			".ssh": ClassSecretLocalConfig, ".aws": ClassSecretLocalConfig, ".gnupg": ClassSecretLocalConfig,
			".cursor/projects": ClassHostPrivateAgentState,
		}[directory], DecisionExclude)
	}
	for _, trap := range traps {
		for _, finding := range proposal.Findings {
			if finding.Path == trap {
				t.Fatalf("scanner descended into private directory %q", trap)
			}
		}
	}
	for _, name := range []string{".ssh", ".aws", ".gnupg"} {
		if !containsValue(proposal.Policy.PrunePatterns, name) {
			t.Fatalf("prune patterns %v do not document %q", proposal.Policy.PrunePatterns, name)
		}
	}
}

func TestScanWithSnapshotAndContentFingerprintDetectMetadataDrift(t *testing.T) {
	mutations := map[string]func(t *testing.T, project string){
		"added file": func(t *testing.T, project string) {
			writeTestFile(t, filepath.Join(project, "added.txt"), "x")
		},
		"removed file": func(t *testing.T, project string) {
			if err := os.Remove(filepath.Join(project, "original.txt")); err != nil {
				t.Fatal(err)
			}
		},
		"renamed file": func(t *testing.T, project string) {
			if err := os.Rename(filepath.Join(project, "original.txt"), filepath.Join(project, "renamed.txt")); err != nil {
				t.Fatal(err)
			}
		},
		"size only": func(t *testing.T, project string) {
			info, err := os.Stat(filepath.Join(project, "original.txt"))
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(project, "original.txt")
			writeTestFile(t, path, "different-size")
			if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
		},
		"mtime only": func(t *testing.T, project string) {
			path := filepath.Join(project, "original.txt")
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			changed := info.ModTime().Add(2 * time.Second)
			if err := os.Chtimes(path, changed, changed); err != nil {
				t.Fatal(err)
			}
		},
		"new secret path": func(t *testing.T, project string) {
			writeTestFile(t, filepath.Join(project, ".ssh", "credentials"), "secret")
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			project := filepath.Join(root, "project")
			writeTestFile(t, filepath.Join(project, "original.txt"), "original")
			options := Options{
				SourceRoot: root,
				Projects:   []ProjectDescriptor{{ID: "project", Path: "project", Kind: ProjectShared}},
				OpenFile: func(string) (io.ReadCloser, error) {
					return nil, errors.New("content fingerprint must not open files")
				},
			}
			_, snapshot, err := ScanWithSnapshot(options)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.ContentFingerprint == "" {
				t.Fatal("scan snapshot has no content fingerprint")
			}
			unchanged, err := ContentFingerprint(options)
			if err != nil {
				t.Fatal(err)
			}
			if unchanged != snapshot.ContentFingerprint {
				t.Fatalf("unchanged fingerprint = %q, want %q", unchanged, snapshot.ContentFingerprint)
			}
			mutate(t, project)
			changed, err := ContentFingerprint(options)
			if err != nil {
				t.Fatal(err)
			}
			if changed == snapshot.ContentFingerprint {
				t.Fatalf("fingerprint did not detect %s", name)
			}
		})
	}
}

func TestScanRejectsProjectSymlinksAndDoesNotFollowNestedSymlinks(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeTestFile(t, filepath.Join(root, "real", "inside.txt"), "inside")
	writeTestFile(t, filepath.Join(outside, "secret.txt"), "outside")
	if err := os.Symlink(outside, filepath.Join(root, "link-project")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "real", "nested-link")); err != nil {
		t.Fatal(err)
	}

	_, err := Scan(Options{SourceRoot: root, Projects: []ProjectDescriptor{{ID: "link", Path: "link-project", Kind: ProjectWorktree}}})
	if err == nil || !strings.Contains(err.Error(), "real directory") {
		t.Fatalf("project symlink error = %v", err)
	}

	proposal, err := Scan(Options{SourceRoot: root, Projects: []ProjectDescriptor{{ID: "real", Path: "real", Kind: ProjectWorktree}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range proposal.Findings {
		if strings.Contains(finding.Path, "secret.txt") {
			t.Fatalf("scanner followed nested symlink: %#v", finding)
		}
	}
}

func TestScanReturnsTypedEntryAndByteLimitErrors(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "project", "one.txt"), "1234")
	writeTestFile(t, filepath.Join(root, "project", "two.txt"), "5678")

	for name, options := range map[string]Options{
		"entries": {SourceRoot: root, Projects: []ProjectDescriptor{{ID: "project", Path: "project", Kind: ProjectShared}}, MaxEntriesPerProject: 1, MaxBytesPerProject: 100},
		"bytes":   {SourceRoot: root, Projects: []ProjectDescriptor{{ID: "project", Path: "project", Kind: ProjectShared}}, MaxEntriesPerProject: 100, MaxBytesPerProject: 5},
	} {
		t.Run(name, func(t *testing.T) {
			proposal, err := Scan(options)
			if proposal != nil {
				t.Fatalf("proposal = %#v, want nil on limit", proposal)
			}
			var limitErr *LimitError
			if !errors.As(err, &limitErr) || limitErr.ProjectID != "project" {
				t.Fatalf("error = %T %v, want project LimitError", err, err)
			}
		})
	}
}

func TestScanResultsAreDeterministic(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "z", "z.txt"), "z")
	writeTestFile(t, filepath.Join(root, "a", "a.txt"), "a")
	options := Options{
		SourceRoot: root,
		Projects: []ProjectDescriptor{
			{ID: "z", Path: "z", Kind: ProjectLocal},
			{ID: "a", Path: "a", Kind: ProjectShared},
		},
		CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	first, err := Scan(options)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Scan(options)
	if err != nil {
		t.Fatal(err)
	}
	firstJSON, _ := MarshalProposal(*first)
	secondJSON, _ := MarshalProposal(*second)
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("scan output differs:\n%s\n%s", firstJSON, secondJSON)
	}
}

func TestScanEmitsGenericAdapterByDefault(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "project", "main.go"), "x")
	proposal, err := Scan(Options{
		SourceRoot: root,
		Projects:   []ProjectDescriptor{{ID: "project", Path: "project", Kind: ProjectShared}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if proposal.Source.Adapter != SourceAdapterGeneric {
		t.Fatalf("adapter = %q, want %q", proposal.Source.Adapter, SourceAdapterGeneric)
	}
}

func TestScanAcceptsWorkspaceManifestAdapter(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "project", "main.go"), "x")
	proposal, err := Scan(Options{
		SourceRoot:    root,
		Projects:      []ProjectDescriptor{{ID: "project", Path: "project", Kind: ProjectShared}},
		SourceAdapter: SourceAdapterWorkspaceManifest,
	})
	if err != nil {
		t.Fatal(err)
	}
	if proposal.Source.Adapter != SourceAdapterWorkspaceManifest {
		t.Fatalf("adapter = %q, want %q", proposal.Source.Adapter, SourceAdapterWorkspaceManifest)
	}
}

func TestScanRejectsDuplicateProjectPaths(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "project", "main.go"), "x")
	_, err := Scan(Options{
		SourceRoot: root,
		Projects: []ProjectDescriptor{
			{ID: "one", Path: "project", Kind: ProjectShared},
			{ID: "two", Path: "project", Kind: ProjectLocal},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate project path") {
		t.Fatalf("duplicate path error = %v", err)
	}
	assertNoAbsolutePath(t, err.Error(), root)
}

func TestScanRejectsContainedDescriptorRootThatDoesNotMatchPortablePath(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "expected", "main.go"), "expected")
	writeTestFile(t, filepath.Join(root, "different", "main.go"), "different")

	_, err := Scan(Options{
		SourceRoot: root,
		Projects: []ProjectDescriptor{{
			ID: "project", Path: "expected", Kind: ProjectShared,
			Root: filepath.Join(root, "different"),
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "portable path") {
		t.Fatalf("mismatched contained root error = %v", err)
	}
	assertNoAbsolutePath(t, err.Error(), root)
}

func TestScanAcceptsMatchingContainedAndApprovedExternalDescriptorRoots(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	writeTestFile(t, filepath.Join(root, "inside", "main.go"), "inside")
	writeTestFile(t, filepath.Join(external, "outside", "main.go"), "outside")

	_, err := Scan(Options{
		SourceRoot: root,
		Projects: []ProjectDescriptor{
			{
				ID: "inside", Path: "inside", Kind: ProjectShared,
				Root: filepath.Join(root, "inside"),
			},
			{
				ID: "outside", Path: "external/outside", Kind: ProjectLocal,
				Root: filepath.Join(external, "outside"),
			},
		},
		ApprovedProjectRoots: []string{external},
	})
	if err != nil {
		t.Fatalf("valid descriptor roots rejected: %v", err)
	}

	_, err = Scan(Options{
		SourceRoot: root,
		Projects: []ProjectDescriptor{{
			ID: "outside", Path: "external/outside", Kind: ProjectLocal,
			Root: filepath.Join(external, "outside"),
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "approved project root") {
		t.Fatalf("unapproved external root error = %v", err)
	}
	assertNoAbsolutePath(t, err.Error(), external)
}

func TestScanManifestIOErrorsHideAbsolutePaths(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "project", "package.json"), `{"scripts":{"test":"vitest"}}`)
	for name, opener := range map[string]OpenFileFunc{
		"open": func(path string) (io.ReadCloser, error) {
			return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrPermission}
		},
		"read": func(path string) (io.ReadCloser, error) {
			return &pathErrorReader{path: path}, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Scan(Options{
				SourceRoot: root,
				Projects:   []ProjectDescriptor{{ID: "project", Path: "project", Kind: ProjectShared}},
				OpenFile:   opener,
			})
			if err == nil {
				t.Fatal("Scan succeeded despite manifest I/O failure")
			}
			assertNoAbsolutePath(t, err.Error(), root)
			if !strings.Contains(err.Error(), "package.json") || !strings.Contains(err.Error(), "project") {
				t.Fatalf("error %q should name the relative manifest path and project ID", err)
			}
			if !errors.Is(err, fs.ErrPermission) {
				t.Fatalf("error %q should keep the permission sentinel", err)
			}
			var pathError *fs.PathError
			if errors.As(err, &pathError) {
				t.Fatalf("error retained an absolute PathError: %#v", pathError)
			}
		})
	}
}

type pathErrorReader struct {
	path string
}

func (reader *pathErrorReader) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: reader.path, Err: fs.ErrPermission}
}

func (*pathErrorReader) Close() error {
	return nil
}

func TestScanWalkErrorsHideAbsolutePaths(t *testing.T) {
	root := t.TempDir()
	privateDirectory := filepath.Join(root, "project", "private")
	writeTestFile(t, filepath.Join(privateDirectory, "file.txt"), "x")
	if err := os.Chmod(privateDirectory, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(privateDirectory, 0o700) })

	_, err := Scan(Options{
		SourceRoot: root,
		Projects:   []ProjectDescriptor{{ID: "project", Path: "project", Kind: ProjectShared}},
	})
	if err == nil {
		t.Skip("filesystem permissions did not block directory traversal")
	}
	assertNoAbsolutePath(t, err.Error(), root)
	if !strings.Contains(err.Error(), "project") || !strings.Contains(err.Error(), "private") {
		t.Fatalf("error %q should name the project and relative path", err)
	}
}

func TestScanValidationErrorsHideAbsolutePaths(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "project", "main.go"), "package main")
	if err := os.WriteFile(filepath.Join(root, "regular-file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := map[string]Options{
		"missing source root": {SourceRoot: filepath.Join(root, "absent"), Projects: []ProjectDescriptor{{ID: "project", Path: "project", Kind: ProjectShared}}},
		"file as source root": {SourceRoot: filepath.Join(root, "regular-file"), Projects: []ProjectDescriptor{{ID: "project", Path: "project", Kind: ProjectShared}}},
		"missing project":     {SourceRoot: root, Projects: []ProjectDescriptor{{ID: "project", Path: "absent", Kind: ProjectShared}}},
		"file as project":     {SourceRoot: root, Projects: []ProjectDescriptor{{ID: "project", Path: "regular-file", Kind: ProjectShared}}},
	}
	for name, options := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Scan(options)
			if err == nil {
				t.Fatal("Scan succeeded on invalid input")
			}
			assertNoAbsolutePath(t, err.Error(), root)
			var pathError *fs.PathError
			if errors.As(err, &pathError) {
				t.Fatalf("error retained an absolute PathError: %#v", pathError)
			}
		})
	}
}

func TestScanManifestParseErrorsHideContentsAndAbsolutePaths(t *testing.T) {
	cases := map[string]string{
		"package.json": `{"scripts": DO_NOT_REPORT`,
		"compose.yaml": "services:\n\t- DO_NOT_REPORT\n  bad indent: [",
		"global.json":  `{"sdk": DO_NOT_REPORT}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeTestFile(t, filepath.Join(root, "project", name), content)
			_, err := Scan(Options{
				SourceRoot: root,
				Projects:   []ProjectDescriptor{{ID: "project", Path: "project", Kind: ProjectShared}},
			})
			if err == nil {
				t.Fatalf("Scan succeeded on malformed %s", name)
			}
			if strings.Contains(err.Error(), "DO_NOT_REPORT") {
				t.Fatalf("error leaked file contents: %v", err)
			}
			assertNoAbsolutePath(t, err.Error(), root)
			if !strings.Contains(err.Error(), name) {
				t.Fatalf("error %q should name the relative manifest path", err)
			}
		})
	}
}

func TestScanProposalOmitsPackageScriptBodies(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "project", "package.json"),
		`{"scripts":{"test":"vitest --token DO_NOT_REPORT"}}`)

	proposal, err := Scan(Options{
		SourceRoot: root,
		Projects:   []ProjectDescriptor{{ID: "project", Path: "project", Kind: ProjectShared}},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := MarshalProposal(*proposal)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "DO_NOT_REPORT") || strings.Contains(string(data), "vitest") {
		t.Fatalf("proposal serialized a script body: %s", data)
	}
	if len(proposal.Commands) != 1 || proposal.Commands[0].Name != "test" {
		t.Fatalf("commands = %#v, want the declared script name only", proposal.Commands)
	}
}

func assertNoAbsolutePath(t *testing.T, message, root string) {
	t.Helper()
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		resolvedRoot = root
	}
	for _, candidate := range []string{root, resolvedRoot} {
		if strings.Contains(message, candidate) {
			t.Fatalf("message %q leaked absolute path %q", message, candidate)
		}
	}
	if strings.Contains(message, os.TempDir()) {
		t.Fatalf("message %q leaked a temporary directory path", message)
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertFindingClass(t *testing.T, findings []Finding, path string, class FindingClass, decision Decision) {
	t.Helper()
	for _, finding := range findings {
		if finding.Path == path {
			if finding.Class != class || finding.Decision != decision {
				t.Fatalf("finding %q = %#v", path, finding)
			}
			return
		}
	}
	t.Fatalf("no finding for %q in %#v", path, findings)
}

func runtimeNames(values []Runtime) []string {
	result := make([]string, len(values))
	for i, value := range values {
		result[i] = value.Name
	}
	return result
}

func commandNames(values []Command) []string {
	result := make([]string, len(values))
	for i, value := range values {
		result[i] = value.Name
	}
	return result
}

func serviceNames(values []Service) []string {
	result := make([]string, len(values))
	for i, value := range values {
		result[i] = value.Name
	}
	return result
}

func containsValue(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
