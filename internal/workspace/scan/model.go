// Package scan provides bounded, metadata-only workspace discovery and
// portable persistence models.
package scan

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"
)

const CurrentSchemaVersion = 1

type ProjectKind string

const (
	ProjectShared   ProjectKind = "shared"
	ProjectLocal    ProjectKind = "local"
	ProjectWorktree ProjectKind = "worktree"
)

type FindingClass string

const (
	ClassSource                FindingClass = "source"
	ClassSecretLocalConfig     FindingClass = "secret_local_config"
	ClassDependency            FindingClass = "dependency"
	ClassGeneratedArtifact     FindingClass = "generated_artifact"
	ClassDatabase              FindingClass = "database"
	ClassDatabaseDump          FindingClass = "database_dump"
	ClassDatabaseScript        FindingClass = "database_script"
	ClassApplicationManifest   FindingClass = "application_manifest"
	ClassServiceManifest       FindingClass = "service_manifest"
	ClassAgentConfig           FindingClass = "agent_config"
	ClassHostPrivateAgentState FindingClass = "host_private_agent_state"
	ClassUnknownLarge          FindingClass = "unknown_large"
)

type Recommendation string

const (
	RecommendationInclude Recommendation = "include"
	RecommendationReview  Recommendation = "review"
	RecommendationExclude Recommendation = "exclude"
)

type Decision string

const (
	DecisionInclude Decision = "include"
	DecisionReview  Decision = "review"
	DecisionExclude Decision = "exclude"
)

type CloudReadiness string

const CloudReadinessLocalOnly CloudReadiness = "local_only"

type SourceAdapter string

const (
	SourceAdapterGeneric           SourceAdapter = "generic"
	SourceAdapterWorkspaceManifest SourceAdapter = "workspace_manifest"
)

type SourceMetadata struct {
	Root    string        `json:"root"`
	Adapter SourceAdapter `json:"adapter"`
}

type Project struct {
	ID   string      `json:"id"`
	Path string      `json:"path"`
	Kind ProjectKind `json:"kind"`
}

type Finding struct {
	Class          FindingClass   `json:"class"`
	ProjectID      string         `json:"projectId"`
	Path           string         `json:"path"`
	Directory      bool           `json:"directory"`
	Size           int64          `json:"size"`
	Reason         string         `json:"reason"`
	Recommendation Recommendation `json:"recommendation"`
	Decision       Decision       `json:"decision"`
}

type Runtime struct {
	ProjectID    string `json:"projectId"`
	Name         string `json:"name"`
	Version      string `json:"version,omitempty"`
	EvidencePath string `json:"evidencePath"`
}

type Command struct {
	ProjectID string `json:"projectId"`
	Name      string `json:"name"`
	Path      string `json:"path"`
}

type Service struct {
	ProjectID string `json:"projectId"`
	Name      string `json:"name"`
	Path      string `json:"path"`
}

type Policy struct {
	Selectors            []string            `json:"selectors"`
	Ignore               []string            `json:"ignore"`
	ProjectIgnore        map[string][]string `json:"projectIgnore"`
	MaxStagingFileSize   string              `json:"maxStagingFileSize,omitempty"`
	MaxEntriesPerProject int64               `json:"maxEntriesPerProject"`
	MaxBytesPerProject   int64               `json:"maxBytesPerProject"`
	PrunePatterns        []string            `json:"prunePatterns"`
}

type Exclusion struct {
	ProjectID string       `json:"projectId"`
	Path      string       `json:"path"`
	Class     FindingClass `json:"class"`
	Reason    string       `json:"reason"`
}

type Proposal struct {
	SchemaVersion            int            `json:"schemaVersion"`
	CreatedAt                time.Time      `json:"createdAt"`
	Generator                string         `json:"generator"`
	Source                   SourceMetadata `json:"source"`
	Projects                 []Project      `json:"projects"`
	Findings                 []Finding      `json:"findings"`
	Runtimes                 []Runtime      `json:"runtimes"`
	Commands                 []Command      `json:"commands"`
	Services                 []Service      `json:"services"`
	Policy                   Policy         `json:"policy"`
	Exclusions               []Exclusion    `json:"exclusions"`
	CloudReadiness           CloudReadiness `json:"cloudReadiness"`
	UnansweredCloudQuestions []string       `json:"unansweredCloudQuestions"`
}

func MarshalProposal(proposal Proposal) ([]byte, error) {
	if err := ValidateProposal(proposal); err != nil {
		return nil, err
	}
	proposal = cloneProposal(proposal)
	normalizeProposal(&proposal)
	return json.MarshalIndent(proposal, "", "  ")
}

func ValidateProposal(proposal Proposal) error {
	if proposal.SchemaVersion != CurrentSchemaVersion {
		return fmt.Errorf("unsupported proposal schema version %d", proposal.SchemaVersion)
	}
	if proposal.CreatedAt.IsZero() {
		return fmt.Errorf("createdAt is required")
	}
	if strings.TrimSpace(proposal.Generator) == "" {
		return fmt.Errorf("generator is required")
	}
	if proposal.Source.Root != "." {
		return fmt.Errorf("source root must be the portable relative path \".\"")
	}
	if !validSourceAdapter(proposal.Source.Adapter) {
		return fmt.Errorf("unsupported source adapter %q", proposal.Source.Adapter)
	}
	if proposal.Projects == nil || proposal.Findings == nil || proposal.Runtimes == nil ||
		proposal.Commands == nil || proposal.Services == nil || proposal.Policy.Selectors == nil ||
		proposal.Policy.Ignore == nil || proposal.Policy.ProjectIgnore == nil ||
		proposal.Policy.PrunePatterns == nil || proposal.Exclusions == nil {
		return fmt.Errorf("proposal collection fields must be present")
	}
	if proposal.UnansweredCloudQuestions == nil {
		return fmt.Errorf("unansweredCloudQuestions must be present")
	}
	if proposal.CloudReadiness != CloudReadinessLocalOnly {
		return fmt.Errorf("cloudReadiness must be %q", CloudReadinessLocalOnly)
	}
	if proposal.Policy.MaxEntriesPerProject <= 0 || proposal.Policy.MaxBytesPerProject <= 0 {
		return fmt.Errorf("policy limits must be positive")
	}
	for project, patterns := range proposal.Policy.ProjectIgnore {
		if !portableRelativePath(project) || patterns == nil {
			return fmt.Errorf("projectIgnore entries must have a project path and present pattern collection")
		}
	}

	projectIDs := make(map[string]struct{}, len(proposal.Projects))
	projectPaths := make(map[string]struct{}, len(proposal.Projects))
	for _, project := range proposal.Projects {
		if project.ID == "" {
			return fmt.Errorf("project ID is required")
		}
		if _, exists := projectIDs[project.ID]; exists {
			return fmt.Errorf("duplicate project ID %q", project.ID)
		}
		if _, exists := projectPaths[project.Path]; exists {
			return fmt.Errorf("duplicate project path %q", project.Path)
		}
		projectIDs[project.ID] = struct{}{}
		projectPaths[project.Path] = struct{}{}
		if !portableRelativePath(project.Path) {
			return fmt.Errorf("project %q path must be a clean relative slash path", project.ID)
		}
		if project.Kind != ProjectShared && project.Kind != ProjectLocal && project.Kind != ProjectWorktree {
			return fmt.Errorf("project %q has invalid kind %q", project.ID, project.Kind)
		}
	}
	for project := range proposal.Policy.ProjectIgnore {
		if _, ok := projectPaths[project]; !ok {
			return fmt.Errorf("projectIgnore references unknown project %q", project)
		}
	}
	for _, finding := range proposal.Findings {
		if _, ok := projectIDs[finding.ProjectID]; !ok {
			return fmt.Errorf("finding references unknown project %q", finding.ProjectID)
		}
		if !portableRelativePath(finding.Path) {
			return fmt.Errorf("finding path must be a clean project-relative slash path")
		}
		if finding.Size < 0 || finding.Reason == "" {
			return fmt.Errorf("finding %q has incomplete metadata", finding.Path)
		}
		if !validClass(finding.Class) || !validRecommendation(finding.Recommendation) || !validDecision(finding.Decision) {
			return fmt.Errorf("finding %q has invalid classification", finding.Path)
		}
	}
	for _, runtime := range proposal.Runtimes {
		if _, ok := projectIDs[runtime.ProjectID]; !ok {
			return fmt.Errorf("runtime references unknown project %q", runtime.ProjectID)
		}
		if runtime.Name == "" || !portableRelativePath(runtime.EvidencePath) {
			return fmt.Errorf("runtime has incomplete or invalid metadata")
		}
	}
	for _, command := range proposal.Commands {
		if _, ok := projectIDs[command.ProjectID]; !ok {
			return fmt.Errorf("command references unknown project %q", command.ProjectID)
		}
		if command.Name == "" || !portableRelativePath(command.Path) {
			return fmt.Errorf("command has incomplete or invalid metadata")
		}
	}
	for _, service := range proposal.Services {
		if _, ok := projectIDs[service.ProjectID]; !ok {
			return fmt.Errorf("service references unknown project %q", service.ProjectID)
		}
		if service.Name == "" || !portableRelativePath(service.Path) {
			return fmt.Errorf("service has incomplete or invalid metadata")
		}
	}
	exclusionKeys := make(map[string]struct{}, len(proposal.Exclusions))
	for _, exclusion := range proposal.Exclusions {
		if _, ok := projectIDs[exclusion.ProjectID]; !ok {
			return fmt.Errorf("exclusion references unknown project %q", exclusion.ProjectID)
		}
		if !portableRelativePath(exclusion.Path) || !validClass(exclusion.Class) || exclusion.Reason == "" {
			return fmt.Errorf("exclusion has incomplete or invalid metadata")
		}
		key := exclusion.ProjectID + "\x00" + exclusion.Path + "\x00" + string(exclusion.Class) + "\x00" + exclusion.Reason
		if _, duplicate := exclusionKeys[key]; duplicate {
			return fmt.Errorf("proposal exclusions contain a duplicate")
		}
		exclusionKeys[key] = struct{}{}
	}
	expectedExclusions := exclusionsFromFindings(proposal.Findings)
	if !reflect.DeepEqual(proposal.Exclusions, expectedExclusions) {
		return fmt.Errorf("proposal exclusions must exactly match excluded finding decisions")
	}
	return nil
}

func RebuildExclusions(proposal *Proposal) {
	proposal.Exclusions = exclusionsFromFindings(proposal.Findings)
}

func exclusionsFromFindings(findings []Finding) []Exclusion {
	exclusions := make([]Exclusion, 0)
	for _, finding := range findings {
		if finding.Decision != DecisionExclude {
			continue
		}
		exclusions = append(exclusions, Exclusion{
			ProjectID: finding.ProjectID,
			Path:      finding.Path,
			Class:     finding.Class,
			Reason:    finding.Reason,
		})
	}
	sort.Slice(exclusions, func(i, j int) bool {
		left, right := exclusions[i], exclusions[j]
		if left.ProjectID != right.ProjectID {
			return left.ProjectID < right.ProjectID
		}
		if left.Path != right.Path {
			return left.Path < right.Path
		}
		if left.Class != right.Class {
			return left.Class < right.Class
		}
		return left.Reason < right.Reason
	})
	return exclusions
}

func cloneProposal(proposal Proposal) Proposal {
	proposal.Projects = cloneSlice(proposal.Projects)
	proposal.Findings = cloneSlice(proposal.Findings)
	proposal.Runtimes = cloneSlice(proposal.Runtimes)
	proposal.Commands = cloneSlice(proposal.Commands)
	proposal.Services = cloneSlice(proposal.Services)
	proposal.Policy.Selectors = cloneSlice(proposal.Policy.Selectors)
	proposal.Policy.Ignore = cloneSlice(proposal.Policy.Ignore)
	proposal.Policy.ProjectIgnore = cloneStringSliceMap(proposal.Policy.ProjectIgnore)
	proposal.Policy.PrunePatterns = cloneSlice(proposal.Policy.PrunePatterns)
	proposal.Exclusions = cloneSlice(proposal.Exclusions)
	proposal.UnansweredCloudQuestions = cloneSlice(proposal.UnansweredCloudQuestions)
	return proposal
}

func cloneStringSliceMap(values map[string][]string) map[string][]string {
	if values == nil {
		return nil
	}
	result := make(map[string][]string, len(values))
	for key, entries := range values {
		result[key] = cloneSlice(entries)
	}
	return result
}

func cloneSlice[T any](values []T) []T {
	if values == nil {
		return nil
	}
	result := make([]T, len(values))
	copy(result, values)
	return result
}

func normalizeProposal(proposal *Proposal) {
	sort.Slice(proposal.Projects, func(i, j int) bool {
		if proposal.Projects[i].Path != proposal.Projects[j].Path {
			return proposal.Projects[i].Path < proposal.Projects[j].Path
		}
		return proposal.Projects[i].ID < proposal.Projects[j].ID
	})
	sort.Slice(proposal.Findings, func(i, j int) bool {
		left, right := proposal.Findings[i], proposal.Findings[j]
		if left.ProjectID != right.ProjectID {
			return left.ProjectID < right.ProjectID
		}
		if left.Path != right.Path {
			return left.Path < right.Path
		}
		return left.Class < right.Class
	})
	sort.Slice(proposal.Runtimes, func(i, j int) bool {
		left, right := proposal.Runtimes[i], proposal.Runtimes[j]
		if left.ProjectID != right.ProjectID {
			return left.ProjectID < right.ProjectID
		}
		if left.Name != right.Name {
			return left.Name < right.Name
		}
		return left.EvidencePath < right.EvidencePath
	})
	sort.Slice(proposal.Commands, func(i, j int) bool {
		left, right := proposal.Commands[i], proposal.Commands[j]
		if left.ProjectID != right.ProjectID {
			return left.ProjectID < right.ProjectID
		}
		return left.Name < right.Name
	})
	sort.Slice(proposal.Services, func(i, j int) bool {
		left, right := proposal.Services[i], proposal.Services[j]
		if left.ProjectID != right.ProjectID {
			return left.ProjectID < right.ProjectID
		}
		return left.Name < right.Name
	})
	sort.Slice(proposal.Exclusions, func(i, j int) bool {
		left, right := proposal.Exclusions[i], proposal.Exclusions[j]
		if left.ProjectID != right.ProjectID {
			return left.ProjectID < right.ProjectID
		}
		if left.Path != right.Path {
			return left.Path < right.Path
		}
		if left.Class != right.Class {
			return left.Class < right.Class
		}
		return left.Reason < right.Reason
	})
	sort.Strings(proposal.Policy.Selectors)
	sort.Strings(proposal.Policy.Ignore)
	for key, values := range proposal.Policy.ProjectIgnore {
		sort.Strings(values)
		proposal.Policy.ProjectIgnore[key] = values
	}
	sort.Strings(proposal.Policy.PrunePatterns)
	sort.Strings(proposal.UnansweredCloudQuestions)
}

func portableRelativePath(path string) bool {
	if path == "" || filepath.IsAbs(path) || strings.Contains(path, `\`) {
		return false
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	return clean == path && path != "." && path != ".." && !strings.HasPrefix(path, "../")
}

func validClass(class FindingClass) bool {
	switch class {
	case ClassSource, ClassSecretLocalConfig, ClassDependency, ClassGeneratedArtifact,
		ClassDatabase, ClassDatabaseDump, ClassDatabaseScript, ClassApplicationManifest, ClassServiceManifest,
		ClassAgentConfig, ClassHostPrivateAgentState, ClassUnknownLarge:
		return true
	default:
		return false
	}
}

func validRecommendation(value Recommendation) bool {
	return value == RecommendationInclude || value == RecommendationReview || value == RecommendationExclude
}

func validDecision(value Decision) bool {
	return value == DecisionInclude || value == DecisionReview || value == DecisionExclude
}

func validSourceAdapter(value SourceAdapter) bool {
	return value == SourceAdapterGeneric || value == SourceAdapterWorkspaceManifest
}
