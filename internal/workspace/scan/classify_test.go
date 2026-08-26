package scan

import (
	"strings"
	"testing"
)

func TestClassifySourceFilesWithSensitiveSoundingNamesStaySource(t *testing.T) {
	for _, path := range []string{
		"secret.go",
		"secrets.ts",
		"credentials.go",
		"credential_provider.py",
		"internal/secrets/manager.go",
		"internal/auth/token.go",
		"pkg/keys/keys.go",
		"src/components/SecretBadge.tsx",
		"docs/credentials.md",
		"tests/secrets_test.go",
	} {
		t.Run(path, func(t *testing.T) {
			got := classifyTestPath(path, false)
			if got.class != ClassSource || got.decision != DecisionInclude {
				t.Fatalf("classify(%q) = %+v, want source include", path, got)
			}
		})
	}
}

func TestClassifySensitiveNamesExtensionsAndLocations(t *testing.T) {
	cases := map[string]struct {
		class    FindingClass
		decision Decision
	}{
		"credentials.json":               {ClassSecretLocalConfig, DecisionExclude},
		"secrets.yaml":                   {ClassSecretLocalConfig, DecisionExclude},
		"service-account.json":           {ClassSecretLocalConfig, DecisionExclude},
		"client.pem":                     {ClassSecretLocalConfig, DecisionExclude},
		"server.key":                     {ClassSecretLocalConfig, DecisionExclude},
		"bundle.crt":                     {ClassSecretLocalConfig, DecisionExclude},
		"store.p12":                      {ClassSecretLocalConfig, DecisionExclude},
		"deploy/id_ed25519":              {ClassSecretLocalConfig, DecisionExclude},
		"home/.ssh/config":               {ClassSecretLocalConfig, DecisionExclude},
		"home/.aws/credentials":          {ClassSecretLocalConfig, DecisionExclude},
		"home/.gnupg/secring.gpg":        {ClassSecretLocalConfig, DecisionExclude},
		".env":                           {ClassSecretLocalConfig, DecisionReview},
		".env.local":                     {ClassSecretLocalConfig, DecisionReview},
		".npmrc":                         {ClassSecretLocalConfig, DecisionReview},
		"appsettings.Local.json":         {ClassSecretLocalConfig, DecisionReview},
		"appsettings.Development.json":   {ClassSource, DecisionInclude},
		"config.local.json":              {ClassSecretLocalConfig, DecisionReview},
		"src/hooks/useLocalState.ts":     {ClassSource, DecisionInclude},
		"src/config/local.controller.go": {ClassSource, DecisionInclude},
		"src/config/app.local.go":        {ClassSource, DecisionInclude},
		".config/nextest.toml":           {ClassSource, DecisionInclude},
	}
	for path, want := range cases {
		t.Run(path, func(t *testing.T) {
			got := classifyTestPath(path, false)
			if got.class != want.class || got.decision != want.decision ||
				(want.decision == DecisionInclude && got.recommendation != RecommendationInclude) {
				t.Fatalf("classify(%q) = %+v, want %s/%s", path, got, want.class, want.decision)
			}
		})
	}
}

func TestClassifyCookieStoreBoundaryMatrix(t *testing.T) {
	cases := []struct {
		path     string
		class    FindingClass
		decision Decision
	}{
		{"profile/Default/Cookies", ClassSecretLocalConfig, DecisionReview},
		{"profile/Default/Cookies-journal", ClassSecretLocalConfig, DecisionReview},
		{"profile/Default/Safe Browsing Cookies", ClassSecretLocalConfig, DecisionReview},
		{"profile/Default/Safe Browsing Cookies-journal", ClassSecretLocalConfig, DecisionReview},
		{"profile/cookies.sqlite", ClassSecretLocalConfig, DecisionReview},
		{"exports/cookies.txt", ClassSecretLocalConfig, DecisionReview},
		{"fixtures/cookies.json", ClassSecretLocalConfig, DecisionReview},
		{"sessions/vendor-session.cookies", ClassSecretLocalConfig, DecisionReview},
		{"utils/cookies.ts", ClassSource, DecisionInclude},
		{"utils/cookies.js", ClassSource, DecisionInclude},
		{"internal/cookies.go", ClassSource, DecisionInclude},
		{"tools/cookies.py", ClassSource, DecisionInclude},
		{"src/cookies.cs", ClassSource, DecisionInclude},
		{"cookie-policy/page.tsx", ClassSource, DecisionInclude},
		{".turbo/cookies/1.cookie", ClassSource, DecisionInclude},
		{"notes/cookies.md", ClassSource, DecisionInclude},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			got := classifyTestPath(tc.path, false)
			if got.class != tc.class || got.decision != tc.decision {
				t.Fatalf("classify(%q) = %+v, want %s/%s", tc.path, got, tc.class, tc.decision)
			}
		})
	}
}

func TestClassifySafeConfigTemplatesAsSource(t *testing.T) {
	for _, path := range []string{
		".env.example",
		".env.sample",
		".env.template",
		".env.example.backup",
		"config/.env.SAMPLE",
		"appsettings.Local.example.json",
		"appsettings.Development.example.json",
	} {
		t.Run(path, func(t *testing.T) {
			got := classifyTestPath(path, false)
			if got.class != ClassSource || got.recommendation != RecommendationInclude || got.decision != DecisionInclude {
				t.Fatalf("classify(%q) = %+v, want source include", path, got)
			}
		})
	}
}

func TestClassifyAppsettingsUsesLocalTokenNotSubstring(t *testing.T) {
	cases := []struct {
		path         string
		class        FindingClass
		decision     Decision
		template     bool
		secretReview bool
	}{
		{path: "appsettings.Development.json", class: ClassSource, decision: DecisionInclude},
		{path: "appsettings.Local.json", class: ClassSecretLocalConfig, decision: DecisionReview, secretReview: true},
		{path: "appsettings.Local.example.json", class: ClassSource, decision: DecisionInclude, template: true},
		{path: "appsettings.Development.example.json", class: ClassSource, decision: DecisionInclude, template: true},
		{path: "appsettings.Localization.json", class: ClassSource, decision: DecisionInclude},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			got := classifyTestPath(tc.path, false)
			if got.class != tc.class || got.decision != tc.decision {
				t.Fatalf("classify(%q) = %+v, want %s/%s", tc.path, got, tc.class, tc.decision)
			}
			if tc.decision == DecisionInclude && got.recommendation != RecommendationInclude {
				t.Fatalf("classify(%q) recommendation = %s, want include", tc.path, got.recommendation)
			}
			if tc.secretReview && (got.class != ClassSecretLocalConfig || got.recommendation != RecommendationReview) {
				t.Fatalf("classify(%q) = %+v, want secret local review", tc.path, got)
			}
			if tc.template && got.reason != "configuration template" {
				t.Fatalf("classify(%q) reason = %q, want configuration template", tc.path, got.reason)
			}
			if !tc.template && !tc.secretReview && strings.Contains(got.reason, "secret") {
				t.Fatalf("classify(%q) reason = %q, want non-secret source", tc.path, got.reason)
			}
		})
	}
}

func TestClassifyDirenvConfigurationDefaultsToSecretLocalReview(t *testing.T) {
	for _, path := range []string{
		".envrc",
		".envrc.local",
		".envrc.private",
		"packages/api/.envrc",
		"config/.ENVRC",
		".direnvrc",
	} {
		t.Run(path, func(t *testing.T) {
			got := classifyTestPath(path, false)
			if got.class != ClassSecretLocalConfig || got.recommendation != RecommendationReview ||
				got.decision != DecisionReview {
				t.Fatalf("classify(%q) = %+v, want secret local config review", path, got)
			}
		})
	}
}

func TestClassifyDirenvTemplatesStaySourceAndMetadataOnly(t *testing.T) {
	for _, path := range []string{
		".envrc.example",
		".envrc.sample",
		".envrc.template",
		"config/.envrc.SAMPLE",
	} {
		t.Run(path, func(t *testing.T) {
			got := classifyTestPath(path, false)
			if got.class != ClassSource || got.recommendation != RecommendationInclude ||
				got.decision != DecisionInclude {
				t.Fatalf("classify(%q) = %+v, want source include", path, got)
			}
			if isSafeManifest(path) {
				t.Fatalf("classify(%q) template reached the safe manifest allowlist", path)
			}
		})
	}
}

func TestClassifyDirenvGeneratedStateIsPrunedBeforeDescent(t *testing.T) {
	got := classifyTestPath(".direnv", true)
	if got.class != ClassGeneratedArtifact || got.decision != DecisionExclude || !got.prune {
		t.Fatalf("classify(.direnv dir) = %+v, want pruned generated artifact exclude", got)
	}
	nested := classifyTestPath("packages/api/.direnv", true)
	if nested.class != ClassGeneratedArtifact || nested.decision != DecisionExclude || !nested.prune {
		t.Fatalf("classify(packages/api/.direnv dir) = %+v, want pruned generated artifact exclude", nested)
	}
	if !containsValue(alwaysPrunedDirectoryNames(), ".direnv") {
		t.Fatalf("prune patterns %v do not document .direnv", alwaysPrunedDirectoryNames())
	}
}

func TestClassifySourceNamesContainingEnvrcStaySource(t *testing.T) {
	for _, path := range []string{
		"envrc.go",
		"envrc_test.go",
		"internal/envrc/loader.go",
		"src/parse_envrc.ts",
		"docs/envrc.md",
		"scripts/direnv-setup.sh",
	} {
		t.Run(path, func(t *testing.T) {
			got := classifyTestPath(path, false)
			if got.class != ClassSource || got.decision != DecisionInclude {
				t.Fatalf("classify(%q) = %+v, want source include", path, got)
			}
		})
	}
	for _, path := range []string{"internal/envrc", "src/direnv"} {
		t.Run("dir/"+path, func(t *testing.T) {
			got := classifyTestPath(path, true)
			if got.prune || got.class != ClassSource || got.decision != DecisionInclude {
				t.Fatalf("classify(%q dir) = %+v, want traversable source", path, got)
			}
		})
	}
}

func TestClassifyAgentStateMatrixSeparatesHostPrivateFromRepositoryOwned(t *testing.T) {
	hostPrivate := []string{
		".mcp.json",
		".cursor/mcp.json",
		".cursor/session-state.json",
		".cursor/projects/sample/agent-transcripts/one.jsonl",
		".cursor/session-env/session.json",
		".cursor/migrations/inventory.json",
		".claude/accounts.json",
		".claude/history.jsonl",
		".claude/sessions/today.json",
		".claude/projects/transcript.jsonl",
		".agent/tokens.json",
		".agent/inventory.json",
		".agent/state.json",
		".agent/migration-inventory.json",
		"packages/app/.claude/history.jsonl",
	}
	for _, path := range hostPrivate {
		t.Run("private/"+path, func(t *testing.T) {
			got := classifyTestPath(path, false)
			if got.class != ClassHostPrivateAgentState || got.decision != DecisionExclude {
				t.Fatalf("classify(%q) = %+v, want host-private exclude", path, got)
			}
		})
	}

	repositoryOwned := []string{
		"AGENTS.md",
		"CLAUDE.md",
		".cursor/rules/style.mdc",
		".cursor/rules/session-handling.mdc",
		".cursor/commands/review.md",
		".claude/commands/session-start.md",
		".claude/skills/build/SKILL.md",
		".agent/instructions.md",
	}
	for _, path := range repositoryOwned {
		t.Run("owned/"+path, func(t *testing.T) {
			got := classifyTestPath(path, false)
			if got.class != ClassAgentConfig || got.decision != DecisionReview {
				t.Fatalf("classify(%q) = %+v, want agent config review", path, got)
			}
		})
	}
	for _, path := range []string{".agent/token.go", ".cursor/state.go"} {
		t.Run("source/"+path, func(t *testing.T) {
			got := classifyTestPath(path, false)
			if got.class != ClassSource || got.decision != DecisionInclude {
				t.Fatalf("classify(%q) = %+v, want source include", path, got)
			}
		})
	}
}

func TestClassifyPrunesOnlyClearlyHostPrivateAgentDirectories(t *testing.T) {
	for _, path := range []string{".claude/sessions", ".claude/token", ".cursor/account", ".agent/inventory"} {
		pruned := classifyTestPath(path, true)
		if pruned.class != ClassHostPrivateAgentState || !pruned.prune {
			t.Fatalf("classify(%q dir) = %+v, want pruned host-private", path, pruned)
		}
	}
	for _, path := range []string{
		".claude", ".cursor", ".cursor/rules", ".claude/commands",
		".cursor/rules/history", ".claude/commands/session",
	} {
		got := classifyTestPath(path, true)
		if got.prune || got.decision != DecisionInclude {
			t.Fatalf("classify(%q dir) = %+v, want traversable include", path, got)
		}
	}
}

func TestClassifyPrunesMachineLocalRuntimeDirectories(t *testing.T) {
	cases := []struct {
		path  string
		class FindingClass
	}{
		{".agent-grid", ClassHostPrivateAgentState},
		{"packages/app/.agent-grid", ClassHostPrivateAgentState},
		{".playwright-data", ClassGeneratedArtifact},
		{".playwright-data-local", ClassGeneratedArtifact},
		{".turbo", ClassGeneratedArtifact},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			got := classifyTestPath(tc.path, true)
			if got.class != tc.class || got.decision != DecisionExclude || !got.prune {
				t.Fatalf("classify(%q dir) = %+v, want pruned %s exclude", tc.path, got, tc.class)
			}
		})
	}
	for _, path := range []string{"vendor", ".playwright-config", "playwright-data"} {
		t.Run("kept/"+path, func(t *testing.T) {
			got := classifyTestPath(path, true)
			if got.prune || got.class != ClassSource || got.decision != DecisionInclude {
				t.Fatalf("classify(%q dir) = %+v, want traversable source", path, got)
			}
		})
	}
}

func TestClassifySQLScriptsDefaultIncludeExceptHighConfidenceDumps(t *testing.T) {
	cases := map[string]struct {
		class    FindingClass
		decision Decision
	}{
		"migrations/0001_init.sql":      {ClassDatabaseScript, DecisionInclude},
		"db/migrate/0002_add_index.sql": {ClassDatabaseScript, DecisionInclude},
		"db/schema.sql":                 {ClassDatabaseScript, DecisionInclude},
		"sql/create_schema.sql":         {ClassDatabaseScript, DecisionInclude},
		"reports/monthly.sql":           {ClassDatabaseScript, DecisionInclude},
		"queries/find_recent_items.sql": {ClassDatabaseScript, DecisionInclude},
		"backup.sql":                    {ClassDatabaseDump, DecisionExclude},
		"nightly-dump.sql":              {ClassDatabaseDump, DecisionExclude},
		"archive/full_backup_2026.sql":  {ClassDatabaseDump, DecisionExclude},
		"snapshots/data.dump":           {ClassDatabaseDump, DecisionExclude},
		"snapshots/data.bak":            {ClassDatabaseDump, DecisionExclude},
		"backups/arbitrary-name.sql":    {ClassDatabaseDump, DecisionExclude},
		"dumps/arbitrary-name.SQL":      {ClassDatabaseDump, DecisionExclude},
	}
	for path, want := range cases {
		t.Run(path, func(t *testing.T) {
			got := classifyTestPath(path, false)
			if got.class != want.class || got.decision != want.decision ||
				(want.decision == DecisionInclude && got.recommendation != RecommendationInclude) {
				t.Fatalf("classify(%q) = %+v, want %s/%s", path, got, want.class, want.decision)
			}
		})
	}
}

func TestClassifyPrunesOnlyKnownDotNetConfigurationSubtrees(t *testing.T) {
	for _, path := range []string{"bin/Debug", "bin/release", "obj/DEBUG", "src/obj/Release"} {
		t.Run("pruned/"+path, func(t *testing.T) {
			got := classifyTestPath(path, true)
			if !got.prune || got.class != ClassGeneratedArtifact || got.decision != DecisionExclude {
				t.Fatalf("classify(%q dir) = %+v, want generated subtree exclude", path, got)
			}
		})
	}
	for _, path := range []string{"bin", "obj", "bin/tools", "obj/source", "src/bin/helpers"} {
		t.Run("kept/"+path, func(t *testing.T) {
			got := classifyTestPath(path, true)
			if got.prune || got.class != ClassSource || got.decision != DecisionInclude {
				t.Fatalf("classify(%q dir) = %+v, want traversable source", path, got)
			}
		})
	}
}

func TestClassifyDirectoryPruningIsConservative(t *testing.T) {
	for _, path := range []string{"internal/build", "build", "target", "src/dist-utils"} {
		t.Run("kept/"+path, func(t *testing.T) {
			got := classifyTestPath(path, true)
			if got.prune || got.class != ClassSource {
				t.Fatalf("classify(%q dir) = %+v, want unpruned source directory", path, got)
			}
		})
	}
	for _, path := range []string{"node_modules", ".git", "__pycache__", ".venv", "dist", "coverage", ".next"} {
		t.Run("pruned/"+path, func(t *testing.T) {
			got := classifyTestPath(path, true)
			if !got.prune || got.decision != DecisionExclude {
				t.Fatalf("classify(%q dir) = %+v, want pruned exclude", path, got)
			}
		})
	}
}

func TestPolicyPrunePatternsMatchClassifierDecisions(t *testing.T) {
	for _, name := range alwaysPrunedDirectoryNames() {
		if got := classifyTestPath(name, true); !got.prune {
			t.Fatalf("policy names %q as pruned but classify returned %+v", name, got)
		}
	}
}

func classifyTestPath(path string, directory bool) classification {
	return classify(entryMetadata{
		relativePath:   path,
		directory:      directory,
		size:           1,
		largeFileBytes: DefaultLargeFileBytes,
	})
}
