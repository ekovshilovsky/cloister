package ignore

// mandatoryPatterns are final and non-negatable. Leaf directory patterns are
// intentionally broad because dependency, output, and cache trees must never
// enter the synchronization data plane.
var mandatoryPatterns = []string{
	".git",
	".hg",
	".svn",
	"node_modules/",
	".pnpm-store/",
	".yarn/cache/",
	".bun/",
	"build/",
	"dist/",
	"out/",
	"target/",
	".next/",
	".nuxt/",
	"coverage/",
	".cache/",
	".turbo/",
	".parcel-cache/",
	".vite/",
	"__pycache__/",
	".pytest_cache/",
	".mypy_cache/",
	".venv/",
	"venv/",
	"*.swp",
	"*.swo",
	"*~",
}

var mandatoryDirectories = map[string]bool{
	".git":          true,
	".hg":           true,
	".svn":          true,
	"node_modules":  true,
	".pnpm-store":   true,
	".bun":          true,
	"build":         true,
	"dist":          true,
	"out":           true,
	"target":        true,
	".next":         true,
	".nuxt":         true,
	"coverage":      true,
	".cache":        true,
	".turbo":        true,
	".parcel-cache": true,
	".vite":         true,
	"__pycache__":   true,
	".pytest_cache": true,
	".mypy_cache":   true,
	".venv":         true,
	"venv":          true,
}

// MandatoryPatterns returns an isolated copy of Cloister's final exclusions.
func MandatoryPatterns() []string {
	return append([]string(nil), mandatoryPatterns...)
}

// MandatoryDirectory reports whether a directory can be pruned before any
// repository rule is read. These names cannot be re-included by user policy.
func MandatoryDirectory(name string) bool {
	return mandatoryDirectories[name]
}
