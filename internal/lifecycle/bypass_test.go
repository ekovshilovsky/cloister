// Proprietary and confidential. All rights reserved.

package lifecycle

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestCommandLayerCannotBypassWorkspaceSeam(t *testing.T) {
	forbidden := []*regexp.Regexp{
		regexp.MustCompile(`\bbackend\.Start\s*\(`),
		regexp.MustCompile(`\bvm\.StartSpec\s*\{`),
		regexp.MustCompile(`\bWorkspaceMount\s*:`),
		regexp.MustCompile(`\bBuildMounts\s*\(`),
		regexp.MustCompile(`\bLocation\s*:\s*workspace\w*`),
	}
	files, err := filepath.Glob(filepath.Join("..", "..", "cmd", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, pattern := range forbidden {
			if pattern.Match(data) {
				t.Errorf("%s contains forbidden workspace-start bypass %q", path, pattern)
			}
		}
	}
}
