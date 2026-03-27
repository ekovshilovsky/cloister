// Package setup provides types and I/O helpers for persisting wizard state and
// resumable progress across interrupted setup sessions. State is stored under
// ~/.cloister/setup/ with one YAML file per profile.
package setup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// SetupState is the top-level structure that captures the outcome of each
// wizard section for a named profile. It is written to
// ~/.cloister/setup/<profile>.yaml after each section completes.
type SetupState struct {
	Version         int             `yaml:"version"`
	CreatedAt       time.Time       `yaml:"created_at"`
	UpdatedAt       time.Time       `yaml:"updated_at"`
	CredentialStore string          `yaml:"credential_store,omitempty"` // "op" or "local"
	Credentials     CredentialState `yaml:"credentials,omitempty"`
	Channels        ChannelState    `yaml:"channels,omitempty"`
	Providers       ProviderState   `yaml:"providers,omitempty"`
	Pairing         PairingState    `yaml:"pairing,omitempty"`
	OAuth           OAuthState      `yaml:"oauth,omitempty"`
}

// CredentialState records which credentials have been provisioned during setup.
// Each boolean field is true once the corresponding secret has been stored in
// the selected credential backend.
type CredentialState struct {
	KeychainPassword   bool `yaml:"keychain_password,omitempty"`
	VMLumeUser         bool `yaml:"vm_lume_user,omitempty"`
	VMOpenClawUser     bool `yaml:"vm_openclaw_user,omitempty"`
	TelegramBotToken   bool `yaml:"telegram_bot_token,omitempty"`
	TelegramUserID     bool `yaml:"telegram_user_id,omitempty"`
	GoogleOAuth        bool `yaml:"google_oauth,omitempty"`
	AnthropicAPIKey    bool `yaml:"anthropic_api_key,omitempty"`
	OpenAIAPIKey       bool `yaml:"openai_api_key,omitempty"`
	GooglePlacesAPIKey bool `yaml:"google_places_api_key,omitempty"`
}

// ChannelState records the configuration status of each supported messaging
// channel that can be used to interact with the OpenClaw agent.
type ChannelState struct {
	Telegram TelegramState `yaml:"telegram,omitempty"`
	WhatsApp WhatsAppState `yaml:"whatsapp,omitempty"`
}

// TelegramState holds the Telegram bot configuration outcome for this profile.
type TelegramState struct {
	Configured  bool   `yaml:"configured,omitempty"`
	BotUsername string `yaml:"bot_username,omitempty"`
}

// WhatsAppState holds the WhatsApp channel configuration outcome for this
// profile. Mode is always "action-only" for the current release; Number is the
// trusted sender phone number used to scope inbound messages.
type WhatsAppState struct {
	Configured bool   `yaml:"configured,omitempty"`
	Mode       string `yaml:"mode,omitempty"`   // "action-only"
	Number     string `yaml:"number,omitempty"` // trusted sender phone number
}

// ProviderState records which LLM provider backends have been configured and
// which one is selected as the default inference provider.
type ProviderState struct {
	Ollama          OllamaState    `yaml:"ollama,omitempty"`
	Anthropic       AnthropicState `yaml:"anthropic,omitempty"`
	DefaultProvider string         `yaml:"default_provider,omitempty"`
}

// OllamaState records the outcome of the local Ollama setup section.
type OllamaState struct {
	Configured   bool   `yaml:"configured,omitempty"`
	Host         string `yaml:"host,omitempty"`
	PrimaryModel string `yaml:"primary_model,omitempty"`
}

// AnthropicState records whether the Anthropic API has been configured for
// this profile.
type AnthropicState struct {
	Configured bool `yaml:"configured,omitempty"`
}

// PairingState records the outcome of the device pairing section, which
// registers the host node and approves client devices.
type PairingState struct {
	NodeHostRegistered bool   `yaml:"node_host_registered,omitempty"`
	NodeDisplayName    string `yaml:"node_display_name,omitempty"`
	DevicesApproved    bool   `yaml:"devices_approved,omitempty"`
}

// OAuthState records which Google service scopes have been authorised via the
// OAuth consent flow during setup.
type OAuthState struct {
	GoogleServices []string `yaml:"google_services,omitempty"`
}

// Progress tracks the wizard's position within the setup sequence, enabling
// the session to resume from the last completed step after an interruption.
// It is persisted to ~/.cloister/setup/<profile>.progress.
type Progress struct {
	CurrentSection string      `yaml:"current_section,omitempty"`
	CurrentStep    string      `yaml:"current_step,omitempty"`
	CompletedSteps []string    `yaml:"completed_steps,omitempty"`
	FailedStep     *FailedStep `yaml:"failed_step,omitempty"`
}

// FailedStep captures the details of a step that terminated with an error,
// providing enough context to surface a meaningful resume prompt to the user.
type FailedStep struct {
	Section   string    `yaml:"section"`
	Step      string    `yaml:"step"`
	Error     string    `yaml:"error"`
	Timestamp time.Time `yaml:"timestamp"`
}

// MarkComplete records section:step as successfully finished. It appends the
// composite key to CompletedSteps, advances CurrentSection and CurrentStep to
// the supplied values, and clears any previously recorded FailedStep.
func (p *Progress) MarkComplete(section, step string) {
	key := section + ":" + step
	p.CompletedSteps = append(p.CompletedSteps, key)
	p.CurrentSection = section
	p.CurrentStep = step
	p.FailedStep = nil
}

// MarkFailed records the step that failed along with the error message and the
// current wall-clock time. The failure is stored so the next session can
// surface an actionable resume prompt rather than restarting from scratch.
func (p *Progress) MarkFailed(section, step, errMsg string) {
	p.FailedStep = &FailedStep{
		Section:   section,
		Step:      step,
		Error:     errMsg,
		Timestamp: time.Now(),
	}
}

// IsComplete reports whether the given section:step combination appears in the
// CompletedSteps list, indicating it does not need to be re-executed on resume.
func (p *Progress) IsComplete(section, step string) bool {
	key := section + ":" + step
	for _, k := range p.CompletedSteps {
		if k == key {
			return true
		}
	}
	return false
}

// StatePath returns the canonical path to the setup state file for the named
// profile: ~/.cloister/setup/<profile>.yaml.
func StatePath(profile string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cloister", "setup", profile+".yaml"), nil
}

// ProgressPath returns the canonical path to the progress file for the named
// profile: ~/.cloister/setup/<profile>.progress.
func ProgressPath(profile string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cloister", "setup", profile+".progress"), nil
}

// LoadState reads and deserialises the YAML state file at path. If the file
// does not exist a zero-valued SetupState is returned without an error, so
// callers can treat a missing file as an empty initial state.
func LoadState(path string) (*SetupState, error) {
	state := &SetupState{}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return state, nil
		}
		return nil, err
	}

	if err := yaml.Unmarshal(data, state); err != nil {
		return nil, err
	}

	return state, nil
}

// SaveState serialises state to YAML and writes it to path using an atomic
// write (write to a sibling .tmp file, then rename). Parent directories are
// created with mode 0700 if absent. CreatedAt is stamped on the first save;
// UpdatedAt is always refreshed to the current time.
func SaveState(path string, state *SetupState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	now := time.Now()
	if state.CreatedAt.IsZero() {
		state.CreatedAt = now
	}
	state.UpdatedAt = now

	data, err := yaml.Marshal(state)
	if err != nil {
		return err
	}

	return atomicWrite(path, data)
}

// LoadProgress reads and deserialises the progress file at path. If the file
// does not exist a zero-valued Progress is returned without an error.
func LoadProgress(path string) (*Progress, error) {
	p := &Progress{}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return p, nil
		}
		return nil, err
	}

	if err := yaml.Unmarshal(data, p); err != nil {
		return nil, err
	}

	return p, nil
}

// SaveProgress serialises p to YAML and writes it to path using an atomic
// write. Parent directories are created with mode 0700 if absent.
func SaveProgress(path string, p *Progress) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	data, err := yaml.Marshal(p)
	if err != nil {
		return err
	}

	return atomicWrite(path, data)
}

// ClearProgress removes the progress file at path. If the file does not exist
// no error is returned, making this safe to call unconditionally at the end of
// a successful setup run.
func ClearProgress(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// atomicWrite writes data to path by first writing to a sibling .tmp file and
// then renaming it into place. On POSIX systems the rename syscall is atomic
// within a single filesystem, ensuring readers never observe a partial write.
func atomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"

	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("writing temporary file %s: %w", tmp, err)
	}

	if err := os.Rename(tmp, path); err != nil {
		// Best-effort cleanup of the orphaned .tmp file.
		_ = os.Remove(tmp)
		return fmt.Errorf("renaming %s to %s: %w", tmp, path, err)
	}

	return nil
}
