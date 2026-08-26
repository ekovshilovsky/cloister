package scan

import (
	"bytes"
	"errors"
	"io"

	"gopkg.in/yaml.v3"
)

// Compose manifests are already capped at maxManifestBytes before parsing.
// These caps bound the syntax tree itself so that a small file cannot force a
// deep or wide traversal.
const (
	maxComposeNodeCount = 20_000
	maxComposeNodeDepth = 32
)

const (
	yamlStringTag = "!!str"
	yamlMergeTag  = "!!merge"
)

// errUnsafeCompose is the single failure value for every rejected Compose
// manifest. Callers translate it into a generic project-relative message, so no
// parser detail, line number, or source fragment can reach a report.
var errUnsafeCompose = errors.New("compose manifest is not a bounded plain mapping")

type composeNodeBudget struct {
	nodes int
}

// composeServiceNames reports the declared top-level service key names of a
// Compose manifest. It reads the manifest as a YAML syntax tree rather than
// decoding it into Go values, so anchors are never expanded and aliases and
// merge keys are rejected instead of resolved. Only key names are returned;
// service values are never read or retained.
func composeServiceNames(data []byte) ([]string, error) {
	document, err := decodeSingleDocument(data)
	if err != nil || document == nil {
		return nil, err
	}
	if err := rejectUnsafeNodes(document, 1, &composeNodeBudget{}); err != nil {
		return nil, err
	}
	if len(document.Content) != 1 {
		return nil, errUnsafeCompose
	}
	root := document.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, errUnsafeCompose
	}
	services, err := uniqueMappingValue(root, "services")
	if err != nil || services == nil {
		return nil, err
	}
	if services.Kind != yaml.MappingNode {
		return nil, errUnsafeCompose
	}

	names := make([]string, 0, len(services.Content)/2)
	seen := make(map[string]struct{}, len(services.Content)/2)
	for i := 0; i < len(services.Content); i += 2 {
		key := services.Content[i]
		if key.Kind != yaml.ScalarNode || key.ShortTag() != yamlStringTag || key.Value == "" {
			return nil, errUnsafeCompose
		}
		if _, duplicate := seen[key.Value]; duplicate {
			return nil, errUnsafeCompose
		}
		seen[key.Value] = struct{}{}
		names = append(names, key.Value)
	}
	return names, nil
}

// decodeSingleDocument returns the one YAML document a Compose manifest may
// contain, or nil when the manifest holds no document at all.
func decodeSingleDocument(data []byte) (*yaml.Node, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		return nil, errUnsafeCompose
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errUnsafeCompose
	}
	return &document, nil
}

// rejectUnsafeNodes walks Content links only. It never follows an alias to its
// anchor, so a self-referential or exponentially fanned-out manifest costs no
// more than the nodes the parser already produced.
func rejectUnsafeNodes(node *yaml.Node, depth int, budget *composeNodeBudget) error {
	if node == nil {
		return errUnsafeCompose
	}
	if depth > maxComposeNodeDepth {
		return errUnsafeCompose
	}
	budget.nodes++
	if budget.nodes > maxComposeNodeCount {
		return errUnsafeCompose
	}
	if node.Kind == yaml.AliasNode {
		return errUnsafeCompose
	}
	if node.Kind == yaml.MappingNode {
		if len(node.Content)%2 != 0 {
			return errUnsafeCompose
		}
		for i := 0; i < len(node.Content); i += 2 {
			if isMergeKey(node.Content[i]) {
				return errUnsafeCompose
			}
		}
	}
	for _, child := range node.Content {
		if err := rejectUnsafeNodes(child, depth+1, budget); err != nil {
			return err
		}
	}
	return nil
}

func isMergeKey(node *yaml.Node) bool {
	return node.Kind == yaml.ScalarNode && node.Value == "<<" &&
		(node.Tag == "" || node.Tag == "!" || node.ShortTag() == yamlMergeTag)
}

// uniqueMappingValue returns the value node for key, rejecting any mapping that
// declares the same key more than once.
func uniqueMappingValue(mapping *yaml.Node, key string) (*yaml.Node, error) {
	var value *yaml.Node
	seen := make(map[string]struct{}, len(mapping.Content)/2)
	for i := 0; i < len(mapping.Content); i += 2 {
		name := mapping.Content[i]
		if name.Kind != yaml.ScalarNode {
			return nil, errUnsafeCompose
		}
		if _, duplicate := seen[name.Value]; duplicate {
			return nil, errUnsafeCompose
		}
		seen[name.Value] = struct{}{}
		if name.Value == key {
			value = mapping.Content[i+1]
		}
	}
	return value, nil
}
