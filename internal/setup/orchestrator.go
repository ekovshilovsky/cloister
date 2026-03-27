package setup

import (
	"github.com/ekovshilovsky/cloister/internal/vm"
	"github.com/spf13/pflag"
)

// SetupContext carries all dependencies needed by section runners. It is
// constructed once per wizard invocation and passed to each section's Run
// function, providing a single, consistent dependency surface without relying
// on global state.
type SetupContext struct {
	Profile      string
	Backend      vm.Backend
	State        *SetupState
	Progress     *Progress
	Creds        CredentialStore
	Interactive  bool
	StatePath    string
	ProgressPath string
}

// Section defines a single wizard section with its check and run functions.
// Each section is responsible for one logical configuration domain (e.g.
// credentials, messaging channels, LLM providers).
type Section struct {
	// Name is the canonical machine-readable identifier for this section,
	// used in CLI flags and progress tracking keys.
	Name string

	// Description is a human-readable summary displayed in the wizard menu
	// and in non-interactive --help output.
	Description string

	// IsConfigured reports whether this section has already been completed
	// for the given state, allowing the wizard to skip it or mark it done
	// in the menu UI.
	IsConfigured func(state *SetupState) bool

	// Run executes the section's configuration steps using the supplied
	// SetupContext. It returns a non-nil error if any step fails and the
	// wizard should halt or surface a resume prompt.
	Run func(ctx *SetupContext) error

	// Flags registers any section-specific CLI flags onto the provided
	// FlagSet. Called during command initialisation so that non-interactive
	// callers can supply all inputs via flags rather than prompts.
	Flags func(fs *pflag.FlagSet)
}

// AllSections returns the ordered list of wizard sections. The order defines
// the linear walkthrough sequence presented to users on first run. Each entry
// must have a unique Name; duplicate names will cause test failures at build
// time.
func AllSections() []Section {
	return []Section{
		{
			Name:         "credentials",
			Description:  "1Password or local credential storage, keychain password, VM user credentials",
			IsConfigured: func(s *SetupState) bool { return s.Credentials.KeychainPassword && s.Credentials.VMLumeUser },
			Run:          runCredentials,
			Flags:        credentialFlags,
		},
		{
			Name:         "channels",
			Description:  "Telegram command channel, WhatsApp action channel",
			IsConfigured: func(s *SetupState) bool { return s.Channels.Telegram.Configured },
			Run:          runChannels,
			Flags:        channelFlags,
		},
		{
			Name:         "providers",
			Description:  "Ollama auto-detection, Anthropic API key, default provider selection",
			IsConfigured: func(s *SetupState) bool { return s.Providers.DefaultProvider != "" },
			Run:          runProviders,
			Flags:        providerFlags,
		},
		{
			Name:         "oauth",
			Description:  "Google OAuth for Gmail, Calendar, Drive, Contacts, Docs, Sheets",
			IsConfigured: func(s *SetupState) bool { return len(s.OAuth.GoogleServices) > 0 },
			Run:          runOAuth,
			Flags:        oauthFlags,
		},
		{
			Name:         "pairing",
			Description:  "Node host registration, trusted proxies, device approval, gateway probe",
			IsConfigured: func(s *SetupState) bool { return s.Pairing.DevicesApproved },
			Run:          runPairing,
			Flags:        pairingFlags,
		},
	}
}

// IsFirstRun reports whether the setup wizard has never completed any section
// for the given state. When true the wizard presents a linear walkthrough;
// when false it presents a menu so the user can jump to any unconfigured
// section.
func IsFirstRun(state *SetupState) bool {
	return !state.Credentials.KeychainPassword &&
		!state.Credentials.VMLumeUser &&
		!state.Channels.Telegram.Configured &&
		state.Providers.DefaultProvider == "" &&
		len(state.OAuth.GoogleServices) == 0 &&
		!state.Pairing.DevicesApproved
}

// Stub section runners — replaced by section_*.go files in subsequent tasks.
func runProviders(ctx *SetupContext) error { return nil }
func runOAuth(ctx *SetupContext) error     { return nil }
func runPairing(ctx *SetupContext) error   { return nil }

// Stub flag registration — replaced by section_*.go files in subsequent tasks.
func providerFlags(fs *pflag.FlagSet) {}
func oauthFlags(fs *pflag.FlagSet)    {}
func pairingFlags(fs *pflag.FlagSet)  {}
