package scan

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestComposeServiceNamesReadsOrdinaryComposeFile(t *testing.T) {
	data := []byte(strings.Join([]string{
		"name: sample",
		"services:",
		"  api:",
		"    build: .",
		"    environment:",
		"      API_TOKEN: DO_NOT_REPORT",
		"    ports:",
		`      - "8080:8080"`,
		"  db:",
		"    image: local",
		"    volumes:",
		"      - data:/var/lib/data",
		"volumes:",
		"  data: {}",
		"networks:",
		"  default:",
		"    driver: bridge",
		"",
	}, "\n"))

	names, err := composeServiceNames(data)
	if err != nil {
		t.Fatalf("ordinary compose file rejected: %v", err)
	}
	if want := []string{"api", "db"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("service names = %v, want %v", names, want)
	}
}

func TestComposeServiceNamesAcceptsFileWithoutServices(t *testing.T) {
	for name, content := range map[string]string{
		"volumes only": "volumes:\n  data: {}\n",
		"empty":        "",
		"comment only": "# nothing here\n",
	} {
		t.Run(name, func(t *testing.T) {
			names, err := composeServiceNames([]byte(content))
			if err != nil {
				t.Fatalf("compose file without services rejected: %v", err)
			}
			if len(names) != 0 {
				t.Fatalf("service names = %v, want none", names)
			}
		})
	}
}

func TestComposeServiceNamesRejectsAliasExpansionBomb(t *testing.T) {
	data := composeAliasExpansionBomb(9, 9)
	if len(data) > 4096 {
		t.Fatalf("bomb fixture is %d bytes, expected a compact source", len(data))
	}

	names, err := composeServiceNames(data)
	if err == nil {
		t.Fatalf("alias expansion bomb accepted with services %v", names)
	}
	if names != nil {
		t.Fatalf("service names = %v, want none on rejection", names)
	}
}

func TestComposeServiceNamesRejectsAliasesAnywhere(t *testing.T) {
	for name, content := range map[string]string{
		"service value": "x-common: &common\n  image: local\nservices:\n  api: *common\n",
		"nested value":  "x-ports: &ports\n  - \"8080:8080\"\nservices:\n  api:\n    ports: *ports\n",
		"service key":   "x-name: &name api\nservices:\n  *name:\n    image: local\n",
		"top level":     "x-services: &services\n  api:\n    image: local\nservices: *services\n",
	} {
		t.Run(name, func(t *testing.T) {
			names, err := composeServiceNames([]byte(content))
			if err == nil {
				t.Fatalf("alias accepted with services %v", names)
			}
		})
	}
}

func TestComposeServiceNamesRejectsMergeKeys(t *testing.T) {
	for name, content := range map[string]string{
		"root level": "x-root: &root\n  services:\n    api:\n      image: local\n<<: *root\n",
		"services level": "x-base: &base\n  api:\n    image: local\nservices:\n  <<: *base\n  db:\n" +
			"    image: local\n",
		"service level": "x-service: &service\n  image: local\nservices:\n  api:\n    <<: *service\n",
		"inline mapping": "x-service: &service\n  image: local\nservices:\n" +
			"  api: {<<: *service, container_name: api}\n",
	} {
		t.Run(name, func(t *testing.T) {
			names, err := composeServiceNames([]byte(content))
			if err == nil {
				t.Fatalf("merge key accepted with services %v", names)
			}
		})
	}
}

func TestComposeServiceNamesRejectsExcessiveNesting(t *testing.T) {
	depth := maxComposeNodeDepth + 4
	data := []byte("services:\n  api:\n    labels: " +
		strings.Repeat("[", depth) + strings.Repeat("]", depth) + "\n")

	if _, err := composeServiceNames(data); err == nil {
		t.Fatal("excessively nested compose file accepted")
	}

	shallow := []byte("services:\n  api:\n    labels: " +
		strings.Repeat("[", 6) + strings.Repeat("]", 6) + "\n")
	names, err := composeServiceNames(shallow)
	if err != nil {
		t.Fatalf("ordinarily nested compose file rejected: %v", err)
	}
	if want := []string{"api"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("service names = %v, want %v", names, want)
	}
}

func TestComposeServiceNamesRejectsExcessiveNodeCount(t *testing.T) {
	items := make([]string, 0, maxComposeNodeCount+1)
	for i := 0; i <= maxComposeNodeCount; i++ {
		items = append(items, "1")
	}
	data := []byte("services:\n  api:\n    labels: [" + strings.Join(items, ",") + "]\n")
	if len(data) > maxManifestBytes {
		t.Fatalf("node count fixture is %d bytes, expected it under the byte cap", len(data))
	}

	if _, err := composeServiceNames(data); err == nil {
		t.Fatal("compose file above the node cap accepted")
	}
}

func TestComposeServiceNamesRejectsDuplicateKeys(t *testing.T) {
	for name, content := range map[string]string{
		"duplicate services key":  "services:\n  api:\n    image: local\nservices:\n  db:\n    image: local\n",
		"duplicate service name":  "services:\n  api:\n    image: local\n  api:\n    image: other\n",
		"duplicate top-level key": "volumes:\n  data: {}\nservices:\n  api:\n    image: local\nvolumes:\n  more: {}\n",
	} {
		t.Run(name, func(t *testing.T) {
			names, err := composeServiceNames([]byte(content))
			if err == nil {
				t.Fatalf("duplicate key accepted with services %v", names)
			}
		})
	}
}

func TestComposeServiceNamesRejectsMalformedShapes(t *testing.T) {
	for name, content := range map[string]string{
		"root sequence":        "- services\n",
		"root scalar":          "services\n",
		"services sequence":    "services:\n  - api\n",
		"services scalar":      "services: api\n",
		"services null":        "services:\n",
		"non scalar key":       "services:\n  ? [api]\n  : image: local\n",
		"multiple documents":   "services:\n  api:\n    image: local\n---\nservices:\n  db:\n    image: local\n",
		"malformed yaml":       "services:\n\t- api\n  bad indent: [\n",
		"empty service name":   "services:\n  \"\":\n    image: local\n",
		"typed non string key": "services:\n  1:\n    image: local\n",
	} {
		t.Run(name, func(t *testing.T) {
			names, err := composeServiceNames([]byte(content))
			if err == nil {
				t.Fatalf("malformed compose shape accepted with services %v", names)
			}
		})
	}
}

func TestComposeServiceNamesNeverRetainsValues(t *testing.T) {
	data := []byte(strings.Join([]string{
		"services:",
		"  api:",
		"    image: registry.invalid/DO_NOT_REPORT",
		"    environment:",
		"      API_TOKEN: DO_NOT_REPORT",
		"",
	}, "\n"))

	names, err := composeServiceNames(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if strings.Contains(name, "DO_NOT_REPORT") {
			t.Fatalf("service name %q retained a manifest value", name)
		}
	}
	if want := []string{"api"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("service names = %v, want %v", names, want)
	}
}

func TestScanComposeFailuresReportSanitizedRelativePathOnly(t *testing.T) {
	cases := map[string]string{
		"alias":             "x-common: &common\n  image: DO_NOT_REPORT\nservices:\n  api: *common\n",
		"merge":             "x-base: &base\n  api:\n    image: DO_NOT_REPORT\nservices:\n  <<: *base\n",
		"alias bomb":        string(composeAliasExpansionBomb(9, 9)),
		"nesting":           "services:\n  api:\n    labels: " + strings.Repeat("[", maxComposeNodeDepth+4) + strings.Repeat("]", maxComposeNodeDepth+4) + "\n",
		"duplicate":         "services:\n  api:\n    image: DO_NOT_REPORT\n  api:\n    image: DO_NOT_REPORT\n",
		"malformed type":    "services: DO_NOT_REPORT\n",
		"malformed yaml":    "services:\n\t- DO_NOT_REPORT\n  bad indent: [\n",
		"multiple document": "services:\n  api:\n    image: DO_NOT_REPORT\n---\nservices:\n  db:\n    image: DO_NOT_REPORT\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeTestFile(t, filepath.Join(root, "project", "compose.yaml"), content)

			proposal, err := Scan(Options{
				SourceRoot: root,
				Projects:   []ProjectDescriptor{{ID: "project", Path: "project", Kind: ProjectShared}},
			})
			if err == nil {
				t.Fatalf("Scan accepted unsafe compose inventory: %#v", proposal.Services)
			}
			want := `parsing service manifest "compose.yaml" in project "project" failed`
			if err.Error() != want {
				t.Fatalf("error = %q, want the generic sanitized message %q", err, want)
			}
			assertNoAbsolutePath(t, err.Error(), root)
			for _, snippet := range []string{"DO_NOT_REPORT", "line ", "yaml:", "<<", "&", "*"} {
				if strings.Contains(err.Error(), snippet) {
					t.Fatalf("error %q leaked source detail %q", err, snippet)
				}
			}
		})
	}
}

func composeAliasExpansionBomb(levels, fanOut int) []byte {
	var builder strings.Builder
	builder.WriteString("x-0: &a0 [\"a\"]\n")
	for level := 1; level <= levels; level++ {
		fmt.Fprintf(&builder, "x-%d: &a%d [", level, level)
		for i := 0; i < fanOut; i++ {
			if i > 0 {
				builder.WriteString(",")
			}
			fmt.Fprintf(&builder, "*a%d", level-1)
		}
		builder.WriteString("]\n")
	}
	fmt.Fprintf(&builder, "services:\n  api:\n    labels: *a%d\n", levels)
	return []byte(builder.String())
}
