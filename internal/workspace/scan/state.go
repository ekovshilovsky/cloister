package scan

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const CurrentFormatVersion = 2

type StateEnvelope struct {
	FormatVersion       int               `json:"formatVersion"`
	Profile             string            `json:"profile"`
	SourceRoot          string            `json:"sourceRoot"`
	ConfigFingerprint   string            `json:"configFingerprint"`
	SourceFingerprint   string            `json:"sourceFingerprint"`
	ContentFingerprint  string            `json:"contentFingerprint"`
	ProjectFingerprints map[string]string `json:"projectFingerprints"`
	ProposalDigest      string            `json:"proposalDigest"`
	Reviewed            bool              `json:"reviewed"`
	ProjectMappings     []ProjectMapping  `json:"projectMappings"`
	Proposal            Proposal          `json:"proposal"`
}

type ProjectMapping struct {
	ProjectID    string `json:"projectId"`
	PortablePath string `json:"portablePath"`
	PhysicalRoot string `json:"physicalRoot"`
}

func ProposalDigest(proposal Proposal) (string, error) {
	data, err := MarshalProposal(proposal)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

// StateMigration converts one older state format into the current envelope.
// Loading never writes a migrated value back to disk.
type StateMigration func(raw json.RawMessage) (StateEnvelope, error)

// ProposalMigration converts one older proposal schema into the current model.
// The original state file remains unchanged.
type ProposalMigration func(raw json.RawMessage) (Proposal, error)

// StateMigrationRegistry remains available for compatible future migrations.
// Version 1 requires a new scan because its project model cannot represent
// repository candidates.
var StateMigrationRegistry = map[int]StateMigration{}

// ProposalMigrationRegistry remains available for compatible future schemas.
// Proposal schema version 1 cannot represent repository candidates.
var ProposalMigrationRegistry = map[int]ProposalMigration{}

func SaveState(path string, state StateEnvelope) error {
	if path == "" {
		return fmt.Errorf("state path is required")
	}
	if state.FormatVersion != CurrentFormatVersion {
		return fmt.Errorf("unsupported state format version %d", state.FormatVersion)
	}
	if err := validateState(state); err != nil {
		return err
	}
	state.Proposal = cloneProposal(state.Proposal)
	normalizeProposal(&state.Proposal)
	if err := ValidateProposal(state.Proposal); err != nil {
		return fmt.Errorf("invalid state proposal: %w", err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding state: %w", err)
	}
	data = append(data, '\n')
	if err := ensurePrivateParent(filepath.Dir(path)); err != nil {
		return err
	}
	return atomicWrite(path, data)
}

func LoadState(path string) (StateEnvelope, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return StateEnvelope{}, fmt.Errorf("reading state: %w", err)
	}
	var header struct {
		FormatVersion *int `json:"formatVersion"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return StateEnvelope{}, fmt.Errorf("decoding state header: %w", err)
	}
	if header.FormatVersion == nil {
		return StateEnvelope{}, fmt.Errorf("state formatVersion is required")
	}
	if *header.FormatVersion != CurrentFormatVersion {
		if *header.FormatVersion > CurrentFormatVersion {
			return StateEnvelope{}, fmt.Errorf("unsupported newer state format version %d", *header.FormatVersion)
		}
		if *header.FormatVersion == 1 {
			return StateEnvelope{}, fmt.Errorf(
				"workspace discovery state format version 1 is obsolete; re-run cloister workspace scan",
			)
		}
		migration, ok := StateMigrationRegistry[*header.FormatVersion]
		if !ok {
			return StateEnvelope{}, fmt.Errorf("missing migration from state format version %d", *header.FormatVersion)
		}
		state, err := migration(data)
		if err != nil {
			return StateEnvelope{}, fmt.Errorf("migrating state format version %d: %w", *header.FormatVersion, err)
		}
		if err := validateState(state); err != nil {
			return StateEnvelope{}, err
		}
		return state, nil
	}

	var rawState struct {
		FormatVersion       int               `json:"formatVersion"`
		Profile             string            `json:"profile"`
		SourceRoot          string            `json:"sourceRoot"`
		ConfigFingerprint   string            `json:"configFingerprint"`
		SourceFingerprint   string            `json:"sourceFingerprint"`
		ContentFingerprint  string            `json:"contentFingerprint"`
		ProjectFingerprints map[string]string `json:"projectFingerprints"`
		ProposalDigest      string            `json:"proposalDigest"`
		Reviewed            bool              `json:"reviewed"`
		ProjectMappings     []ProjectMapping  `json:"projectMappings"`
		Proposal            json.RawMessage   `json:"proposal"`
	}
	if err := decodeStrictJSON(data, &rawState); err != nil {
		return StateEnvelope{}, fmt.Errorf("decoding state: %w", err)
	}
	if len(rawState.Proposal) == 0 || bytes.Equal(rawState.Proposal, []byte("null")) {
		return StateEnvelope{}, fmt.Errorf("state proposal is required")
	}

	var proposalHeader struct {
		SchemaVersion *int `json:"schemaVersion"`
	}
	if err := json.Unmarshal(rawState.Proposal, &proposalHeader); err != nil {
		return StateEnvelope{}, fmt.Errorf("decoding proposal header: %w", err)
	}
	if proposalHeader.SchemaVersion == nil {
		return StateEnvelope{}, fmt.Errorf("proposal schemaVersion is required")
	}
	var proposal Proposal
	if *proposalHeader.SchemaVersion > CurrentSchemaVersion {
		return StateEnvelope{}, fmt.Errorf("unsupported newer proposal schema version %d", *proposalHeader.SchemaVersion)
	}
	if *proposalHeader.SchemaVersion < CurrentSchemaVersion {
		if *proposalHeader.SchemaVersion == 1 {
			return StateEnvelope{}, fmt.Errorf(
				"workspace proposal schema version 1 is obsolete; re-run cloister workspace scan",
			)
		}
		migration, ok := ProposalMigrationRegistry[*proposalHeader.SchemaVersion]
		if !ok {
			return StateEnvelope{}, fmt.Errorf("missing migration from proposal schema version %d", *proposalHeader.SchemaVersion)
		}
		proposal, err = migration(rawState.Proposal)
		if err != nil {
			return StateEnvelope{}, fmt.Errorf("migrating proposal schema version %d: %w", *proposalHeader.SchemaVersion, err)
		}
	} else {
		var required struct {
			Source map[string]json.RawMessage `json:"source"`
		}
		if err := json.Unmarshal(rawState.Proposal, &required); err != nil {
			return StateEnvelope{}, fmt.Errorf("decoding proposal fields: %w", err)
		}
		if _, present := required.Source["adapter"]; !present {
			return StateEnvelope{}, fmt.Errorf("proposal source adapter field is required")
		}
		if err := decodeStrictJSON(rawState.Proposal, &proposal); err != nil {
			return StateEnvelope{}, fmt.Errorf("decoding proposal: %w", err)
		}
	}
	state := StateEnvelope{
		FormatVersion: rawState.FormatVersion, Profile: rawState.Profile, SourceRoot: rawState.SourceRoot,
		ConfigFingerprint: rawState.ConfigFingerprint, ProposalDigest: rawState.ProposalDigest,
		SourceFingerprint: rawState.SourceFingerprint, Reviewed: rawState.Reviewed,
		ContentFingerprint:  rawState.ContentFingerprint,
		ProjectFingerprints: rawState.ProjectFingerprints,
		ProjectMappings:     rawState.ProjectMappings, Proposal: proposal,
	}
	if err := validateState(state); err != nil {
		return StateEnvelope{}, err
	}
	normalizeProposal(&state.Proposal)
	return state, nil
}

func decodeStrictJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	return requireJSONEOF(decoder)
}

func validateState(state StateEnvelope) error {
	if state.FormatVersion != CurrentFormatVersion {
		return fmt.Errorf("unsupported state format version %d", state.FormatVersion)
	}
	if state.Profile == "" || filepath.Base(state.Profile) != state.Profile || state.Profile == "." || state.Profile == ".." ||
		strings.ContainsAny(state.Profile, `/\`+"\x00") {
		return fmt.Errorf("state profile is required and must be safe")
	}
	if err := validateCanonicalDirectory(state.SourceRoot); err != nil {
		return fmt.Errorf("state sourceRoot must be canonical: %w", err)
	}
	if state.ConfigFingerprint == "" || state.SourceFingerprint == "" ||
		state.ContentFingerprint == "" || state.ProposalDigest == "" {
		return fmt.Errorf("state fingerprints are required")
	}
	if state.ProjectFingerprints == nil || len(state.ProjectFingerprints) != len(state.Proposal.Projects) {
		return fmt.Errorf("state project fingerprints must correspond to proposal projects")
	}
	if err := ValidateProposal(state.Proposal); err != nil {
		return fmt.Errorf("invalid state proposal: %w", err)
	}
	digest, err := ProposalDigest(state.Proposal)
	if err != nil {
		return fmt.Errorf("digesting state proposal: %w", err)
	}
	if digest != state.ProposalDigest {
		return fmt.Errorf("state proposal digest mismatch")
	}
	if state.ProjectMappings == nil || len(state.ProjectMappings) != len(state.Proposal.Projects) {
		return fmt.Errorf("state project mappings must correspond to proposal projects")
	}
	projects := make(map[string]string, len(state.Proposal.Projects))
	for _, project := range state.Proposal.Projects {
		projects[project.ID] = project.Path
		if state.ProjectFingerprints[project.ID] == "" {
			return fmt.Errorf("state project fingerprints must correspond to proposal projects")
		}
	}
	for projectID := range state.ProjectFingerprints {
		if _, exists := projects[projectID]; !exists {
			return fmt.Errorf("state project fingerprints must correspond to proposal projects")
		}
	}
	ids := make(map[string]bool, len(state.ProjectMappings))
	paths := make(map[string]bool, len(state.ProjectMappings))
	for _, mapping := range state.ProjectMappings {
		if mapping.ProjectID == "" || strings.ContainsAny(mapping.ProjectID, `\`+"\x00") ||
			strings.Contains(mapping.ProjectID, "//") || !portableProjectPath(mapping.PortablePath) {
			return fmt.Errorf("state project mapping has invalid profile/path fields")
		}
		if ids[mapping.ProjectID] || paths[mapping.PortablePath] {
			return fmt.Errorf("state project mappings contain duplicate IDs or paths")
		}
		ids[mapping.ProjectID], paths[mapping.PortablePath] = true, true
		if projects[mapping.ProjectID] != mapping.PortablePath {
			return fmt.Errorf("state project mapping does not correspond to proposal")
		}
		if err := validateCanonicalDirectory(mapping.PhysicalRoot); err != nil {
			return fmt.Errorf("state project physical root must be canonical: %w", err)
		}
	}
	if state.Reviewed {
		for _, project := range state.Proposal.Projects {
			if project.Decision == DecisionReview {
				return fmt.Errorf("reviewed state has unresolved project decisions")
			}
		}
		for _, finding := range state.Proposal.Findings {
			if finding.Decision == DecisionReview {
				return fmt.Errorf("reviewed state has unresolved decisions")
			}
		}
	}
	return nil
}

func validateCanonicalDirectory(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsRune(path, '\x00') {
		return fmt.Errorf("path must be a clean absolute path")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	absolute, err := filepath.Abs(resolved)
	if err != nil || absolute != path {
		return fmt.Errorf("path is not canonical")
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("path is not a directory")
	}
	return nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if err == io.EOF {
		return nil
	}
	if err == nil {
		return fmt.Errorf("decoding state: multiple JSON values")
	}
	return fmt.Errorf("decoding state: %w", err)
}

func ensurePrivateParent(parent string) error {
	if _, err := os.Stat(parent); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("reading state parent: %w", err)
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("creating state parent: %w", err)
	}
	if err := os.Chmod(parent, 0o700); err != nil {
		return fmt.Errorf("setting state parent permissions: %w", err)
	}
	return nil
}

func atomicWrite(path string, data []byte) (resultErr error) {
	parent := filepath.Dir(path)
	file, err := os.CreateTemp(parent, "."+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("creating temporary state file: %w", err)
	}
	tempPath := file.Name()
	defer func() {
		if resultErr != nil {
			_ = os.Remove(tempPath)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return fmt.Errorf("setting temporary state permissions: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("writing temporary state: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("syncing temporary state: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("closing temporary state: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replacing state file: %w", err)
	}
	return nil
}
