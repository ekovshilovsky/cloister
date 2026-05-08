package linux

// stackDependencies records implicit one-way dependencies between cloister
// stacks. When a key stack is requested, every value stack is installed
// alongside it in iteration order, after the key. The relationship is one-way
// only: installing the value stack on its own does not pull in the key.
//
// The current entry — web → art — reflects that web work routinely needs the
// art stack's image, SVG, and document tooling for icon generation, asset
// optimization, screenshot processing, etc., and forgetting to list it
// alongside web is a recurring papercut.
var stackDependencies = map[string][]string{
	"web": {"art"},
}

// expandStackDependencies returns the requested stacks in their original
// order with implicit dependencies inserted immediately after the stack that
// pulled them in. Duplicates (whether already in the requested list or
// introduced via dependency resolution) are dropped, so the returned slice
// is deduplicated and the script for any given stack runs exactly once even
// when it is both explicitly requested and implied by another stack.
func expandStackDependencies(requested []string) []string {
	seen := make(map[string]struct{}, len(requested))
	out := make([]string, 0, len(requested))
	add := func(name string) {
		if _, dup := seen[name]; dup {
			return
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	for _, s := range requested {
		add(s)
		for _, dep := range stackDependencies[s] {
			add(dep)
		}
	}
	return out
}
