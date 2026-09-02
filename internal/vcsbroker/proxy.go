package vcsbroker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"cloister.io/internal/broker"
)

// Request is one argv-preserving guest command request.
type Request struct {
	Tool string
	CWD  string
	Args []string
	Env  []string
}

// HostCommandRunner is the process seam used by unit tests.
type HostCommandRunner interface {
	Run(context.Context, string, []string, string, []string, io.Writer) (int, error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, executable string, args []string, dir string, env []string, output io.Writer) (int, error) {
	path, err := exec.LookPath(executable)
	if err != nil {
		return 127, fmt.Errorf("host executable %q was not found: %w", executable, err)
	}
	command := exec.CommandContext(ctx, path, args...)
	command.Dir = dir
	command.Env = env
	command.Stdin = nil
	command.Stdout = output
	command.Stderr = output
	err = command.Run()
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	return 125, err
}

// Proxy establishes synchronization barriers and runs host VCS commands.
type Proxy struct {
	Broker broker.SyncBroker
	Mapper *Mapper
	Runner HostCommandRunner

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// NewProxy constructs a host command proxy with per-project serialization.
func NewProxy(syncBroker broker.SyncBroker, mapper *Mapper, runner HostCommandRunner) *Proxy {
	if runner == nil {
		runner = execRunner{}
	}
	return &Proxy{Broker: syncBroker, Mapper: mapper, Runner: runner, locks: make(map[string]*sync.Mutex)}
}

// Execute runs one constrained command with the required flush barriers.
func (p *Proxy) Execute(ctx context.Context, request Request, output io.Writer) (int, error) {
	if p == nil || p.Broker == nil || p.Mapper == nil || p.Runner == nil {
		return 125, fmt.Errorf("VCS proxy is not fully configured")
	}
	if request.Tool != "git" && request.Tool != "gh" {
		return 125, fmt.Errorf("unsupported VCS executable %q", request.Tool)
	}
	if err := validateFields(request); err != nil {
		return 125, err
	}

	guestCWD, args, err := effectiveGuestCommand(request)
	if err != nil {
		return 2, err
	}
	scope, err := validateCommand(request.Tool, args, request.Env)
	if err != nil {
		return 2, err
	}
	if scope == commandScopeAccount {
		return p.executeHostOnly(ctx, request, args, output)
	}
	mapping, err := p.Mapper.MapGuest(guestCWD)
	if err != nil {
		if request.Tool == "gh" && ghHasExplicitRepo(args, request.Env) {
			return p.executeHostOnly(ctx, request, args, output)
		}
		if request.Tool == "git" {
			return 2, fmt.Errorf("%v; use git -C ~/workspaces/<project> <command>", err)
		}
		return 125, fmt.Errorf("%v; use gh -R owner/repo <command> or cd to ~/workspaces/<project>", err)
	}

	lock := p.projectLock(mapping.Spec.ProjectID)
	lock.Lock()
	defer lock.Unlock()

	if err := p.barrier(ctx, mapping.Spec); err != nil {
		return 125, fmt.Errorf("pre-command workspace barrier: %w", err)
	}
	hostCWD, err := p.Mapper.ResolveHost(mapping)
	if err != nil {
		return 125, err
	}
	// Only git takes absolute guest filesystem paths as arguments that must be
	// remapped to the host. gh arguments are API endpoints and flags (for
	// example `gh api /repos/o/r`), not host paths, so remapping them would
	// wrongly reject an ordinary endpoint as escaping the project. gh file
	// arguments are resolved relative to the mapped host working directory.
	if request.Tool == "git" {
		args, err = rewriteMappedAbsoluteArgs(p.Mapper, mapping.Spec.ProjectID, args)
		if err != nil {
			return 2, err
		}
	}

	exitCode, runErr := p.Runner.Run(ctx, request.Tool, args, hostCWD, commandEnvironment(request.Env), output)
	mutates := mutatesWorkingTree(request.Tool, args)
	if mutates {
		if barrierErr := p.barrier(ctx, mapping.Spec); barrierErr != nil {
			return 125, fmt.Errorf("post-command workspace barrier after %s exit %d: %w", request.Tool, exitCode, barrierErr)
		}
	}
	if runErr != nil {
		return 125, fmt.Errorf("starting host %s: %w", request.Tool, runErr)
	}
	return exitCode, nil
}

// executeHostOnly runs commands whose GitHub context is fully independent of
// a local checkout. Validation is the gate to this path: account-scoped gh
// commands mirror GitHub CLI commands that do not call Factory.BaseRepo(), and
// repo-scoped commands can only enter with an explicit -R/--repo or GH_REPO.
// With no mapped project, there is intentionally no project lock or Mutagen
// barrier to acquire.
func (p *Proxy) executeHostOnly(ctx context.Context, request Request, args []string, output io.Writer) (int, error) {
	hostCWD, err := os.UserHomeDir()
	if err != nil {
		return 125, fmt.Errorf("resolving host home for %s: %w", request.Tool, err)
	}
	exitCode, runErr := p.Runner.Run(ctx, request.Tool, args, hostCWD, commandEnvironment(request.Env), output)
	if runErr != nil {
		return 125, fmt.Errorf("starting host %s: %w", request.Tool, runErr)
	}
	return exitCode, nil
}

func (p *Proxy) barrier(ctx context.Context, spec broker.SessionSpec) error {
	if err := p.Broker.Flush(ctx, spec); err != nil {
		return err
	}
	status, err := p.Broker.Status(ctx, spec)
	if err != nil {
		return err
	}
	return status.Clean()
}

func (p *Proxy) projectLock(id string) *sync.Mutex {
	p.mu.Lock()
	defer p.mu.Unlock()
	lock := p.locks[id]
	if lock == nil {
		lock = &sync.Mutex{}
		p.locks[id] = lock
	}
	return lock
}

func validateFields(request Request) error {
	if strings.IndexByte(request.CWD, 0) >= 0 || len(request.CWD) > 16*1024 {
		return fmt.Errorf("invalid guest working directory")
	}
	if len(request.Args) > 1024 {
		return fmt.Errorf("too many VCS arguments")
	}
	for _, arg := range request.Args {
		if strings.IndexByte(arg, 0) >= 0 || len(arg) > 1024*1024 {
			return fmt.Errorf("invalid VCS argument")
		}
	}
	return nil
}

func effectiveGuestCommand(request Request) (string, []string, error) {
	args := append([]string(nil), request.Args...)
	if request.Tool != "git" {
		return request.CWD, args, nil
	}
	cwd := request.CWD
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-C" {
			if i+1 >= len(args) {
				return "", nil, fmt.Errorf("git -C requires a directory")
			}
			i++
			cwd = guestPath(cwd, args[i])
			args = append(args[:i-1], args[i+1:]...)
			i -= 2
			continue
		}
		if strings.HasPrefix(arg, "-C") && len(arg) > 2 {
			cwd = guestPath(cwd, arg[2:])
			args = append(args[:i], args[i+1:]...)
			i--
			continue
		}
		if arg == "--git-dir" || arg == "--work-tree" || arg == "--config-env" || arg == "-c" ||
			arg == "--exec-path" || arg == "--namespace" ||
			strings.HasPrefix(arg, "--git-dir=") || strings.HasPrefix(arg, "--work-tree=") ||
			strings.HasPrefix(arg, "--config-env=") || strings.HasPrefix(arg, "--exec-path=") ||
			strings.HasPrefix(arg, "--namespace=") ||
			(strings.HasPrefix(arg, "-c") && !strings.HasPrefix(arg, "--")) {
			// --exec-path/--namespace are denied alongside -c/--git-dir because
			// they can redirect git to guest-controlled binaries or repositories.
			return "", nil, fmt.Errorf("git option %q is not available through the host VCS broker", arg)
		}
		if !strings.HasPrefix(arg, "-") || arg == "--" {
			break
		}
	}
	return cwd, args, nil
}

func guestPath(cwd, value string) string {
	if filepath.IsAbs(value) {
		return filepath.Clean(value)
	}
	return filepath.Clean(filepath.Join(cwd, value))
}

var allowedGitCommands = map[string]bool{
	"add": true, "am": true, "apply": true, "bisect": true, "blame": true,
	"branch": true, "cat-file": true, "check-attr": true, "check-ignore": true,
	"checkout": true, "cherry-pick": true, "clean": true, "commit": true,
	"describe": true, "diff": true, "fetch": true,
	"grep": true, "log": true, "ls-files": true, "ls-tree": true,
	"merge": true, "merge-base": true, "mv": true, "name-rev": true,
	"pull": true, "push": true, "rebase": true, "reflog": true,
	"remote": true, "reset": true, "restore": true, "revert": true, "rev-list": true,
	"rev-parse": true, "rm": true, "show": true, "show-ref": true, "sparse-checkout": true,
	"stash": true, "status": true, "submodule": true, "switch": true, "tag": true,
	"worktree": true,
}

type commandScope uint8

const (
	commandScopeProject commandScope = iota
	commandScopeAccount
)

// This table mirrors GitHub CLI's own Factory.BaseRepo() split. Commands
// marked account-scoped do not ask gh to discover a repository from the
// working directory; api and repo are refined from their operands.
var allowedGHCommands = map[string]commandScope{
	"api": commandScopeAccount, "auth": commandScopeAccount,
	"issue": commandScopeProject, "pr": commandScopeProject,
	"release": commandScopeProject, "repo": commandScopeProject,
	"run": commandScopeProject, "search": commandScopeAccount,
	"status": commandScopeAccount, "workflow": commandScopeProject,
}

func validateCommand(tool string, args, env []string) (commandScope, error) {
	command, rest := subcommand(args)
	if tool == "gh" {
		command, rest = ghSubcommand(args)
	}
	if command == "" {
		if tool == "gh" {
			return commandScopeAccount, nil
		}
		return commandScopeProject, nil
	}
	if tool == "git" {
		if !allowedGitCommands[command] {
			return commandScopeProject, fmt.Errorf("git subcommand %q is not allowed through the host VCS broker", command)
		}
		if command == "commit" && !hasCommitMessage(rest) && !safeEditor(env) {
			return commandScopeProject, fmt.Errorf("interactive commit editors are unavailable through the VCS broker; pass -m, -F, or --no-edit")
		}
		if (command == "add" || command == "checkout" || command == "reset" || command == "stash") && hasAny(rest, "-p", "--patch", "-i", "--interactive") {
			return commandScopeProject, fmt.Errorf("interactive patch selection is unavailable through the VCS broker; run this command on the host")
		}
		if command == "rebase" && hasAny(rest, "-i", "--interactive") {
			return commandScopeProject, fmt.Errorf("interactive rebase is unavailable through the VCS broker; run it on the host")
		}
		if command == "rebase" && hasAny(rest, "-x", "--exec") {
			return commandScopeProject, fmt.Errorf("git rebase --exec is unavailable through the host VCS broker")
		}
		if command == "submodule" && firstOperand(rest) == "foreach" {
			return commandScopeProject, fmt.Errorf("git submodule foreach is unavailable through the host VCS broker")
		}
		if command == "bisect" && firstOperand(rest) == "run" {
			return commandScopeProject, fmt.Errorf("git bisect run is unavailable through the host VCS broker")
		}
		if command == "worktree" && firstOperand(rest) != "list" {
			return commandScopeProject, fmt.Errorf("mutating git worktree commands are unavailable through the VCS broker; create worktrees on the host")
		}
		for _, arg := range rest {
			pathValue := arg
			if _, value, ok := strings.Cut(arg, "="); ok {
				pathValue = value
			} else if strings.HasPrefix(arg, "-F") && len(arg) > 2 {
				pathValue = arg[2:]
			}
			clean := filepath.Clean(pathValue)
			if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || strings.Contains(arg, "/../") {
				return commandScopeProject, fmt.Errorf("path argument %q escapes the mapped workspace project", arg)
			}
			if arg == "--no-index" || arg == "--unsafe-paths" || strings.HasPrefix(arg, "--open-files-in-pager") {
				return commandScopeProject, fmt.Errorf("git option %q is unavailable through the host VCS broker", arg)
			}
		}
		return commandScopeProject, nil
	}
	if command == "alias" || command == "extension" {
		return commandScopeProject, fmt.Errorf("gh %s is unavailable through the host VCS broker", command)
	}
	scope, allowed := allowedGHCommands[command]
	if !allowed {
		return commandScopeProject, fmt.Errorf("gh subcommand %q is not allowed through the host VCS broker", command)
	}
	if command == "api" && ghAPIWrites(rest) {
		// gh runs host-side with the host's GitHub credentials, so an
		// unrestricted `gh api` write would let untrusted guest code mutate the
		// whole account (add SSH keys, delete repos, add collaborators) far
		// beyond the mapped project. Only read-only (GET, no field/input/method)
		// requests are proxied; run write API calls on the host.
		return commandScopeProject, fmt.Errorf("only read-only gh api requests are available through the host VCS broker; run write API calls on the host")
	}
	if command == "auth" && firstGHOperand(rest) != "status" {
		return commandScopeProject, fmt.Errorf("only gh auth status is available through the host VCS broker")
	}
	if command == "repo" {
		action := firstGHOperand(rest)
		if action != "view" && action != "list" {
			return commandScopeProject, fmt.Errorf("only gh repo view and gh repo list are available through the host VCS broker")
		}
		if action == "list" {
			scope = commandScopeAccount
		}
	}
	if command == "api" && ghAPIEndpointNeedsRepo(rest) {
		scope = commandScopeProject
	}
	for _, arg := range rest {
		pathValue := arg
		if _, value, ok := strings.Cut(arg, "="); ok {
			pathValue = value
		}
		clean := filepath.Clean(pathValue)
		if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || strings.Contains(pathValue, "/../") {
			return commandScopeProject, fmt.Errorf("path argument %q escapes the mapped workspace project", arg)
		}
	}
	return scope, nil
}

func ghSubcommand(args []string) (string, []string) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			if i+1 < len(args) {
				return args[i+1], args[i+2:]
			}
			return "", nil
		}
		if arg == "-R" || arg == "--repo" {
			i++
			continue
		}
		if strings.HasPrefix(arg, "-R") || strings.HasPrefix(arg, "--repo=") || strings.HasPrefix(arg, "-") {
			continue
		}
		return arg, args[i+1:]
	}
	return "", nil
}

func firstGHOperand(args []string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == "-R" || args[i] == "--repo" {
			i++
			continue
		}
		if strings.HasPrefix(args[i], "-R") || strings.HasPrefix(args[i], "--repo=") {
			continue
		}
		if !strings.HasPrefix(args[i], "-") {
			return args[i]
		}
	}
	return ""
}

func ghAPIEndpointNeedsRepo(args []string) bool {
	endpoint := ghAPIEndpoint(args)
	if strings.Contains(endpoint, "{owner}") || strings.Contains(endpoint, "{repo}") || strings.Contains(endpoint, "{branch}") {
		return true
	}
	path := endpoint
	if parsed, err := url.Parse(endpoint); err == nil && parsed.IsAbs() {
		path = parsed.Path
	}
	return strings.HasPrefix(strings.TrimLeft(path, "/"), "repos/")
}

func ghAPIEndpoint(args []string) string {
	valueOptions := map[string]bool{
		"-H": true, "--header": true, "--hostname": true, "--cache": true,
		"-X": true, "--method": true, "-f": true, "--raw-field": true,
		"-F": true, "--field": true, "--input": true, "--preview": true,
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		}
		if valueOptions[arg] {
			i++
			continue
		}
		if strings.HasPrefix(arg, "-") {
			continue
		}
		return arg
	}
	return ""
}

func ghHasExplicitRepo(args, env []string) bool {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if arg == "-R" || arg == "--repo" {
			return i+1 < len(args) && args[i+1] != ""
		}
		if strings.HasPrefix(arg, "--repo=") {
			return strings.TrimPrefix(arg, "--repo=") != ""
		}
		if strings.HasPrefix(arg, "-R") && len(arg) > 2 {
			return strings.TrimPrefix(strings.TrimPrefix(arg, "-R"), "=") != ""
		}
	}
	for i := len(env) - 1; i >= 0; i-- {
		value := env[i]
		if strings.HasPrefix(value, "GH_REPO=") && strings.TrimPrefix(value, "GH_REPO=") != "" {
			return true
		}
		if strings.HasPrefix(value, "GH_REPO=") {
			return false
		}
	}
	return false
}

// ghAPIWrites reports whether a `gh api` invocation would mutate server state.
// gh api defaults to GET but becomes a write when given a non-GET method or any
// request body field, so the presence of any of those markers is treated as a
// write and rejected. GraphQL (-f query=...) is also treated as a write because
// its POST body can carry a mutation and cannot be distinguished here.
func ghAPIWrites(args []string) bool {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-X" || arg == "--method":
			if i+1 < len(args) && !strings.EqualFold(args[i+1], "GET") {
				return true
			}
		case strings.HasPrefix(arg, "-X"):
			if v := strings.TrimPrefix(arg, "-X"); v != "" && !strings.EqualFold(v, "GET") {
				return true
			}
		case strings.HasPrefix(arg, "--method="):
			if !strings.EqualFold(strings.TrimPrefix(arg, "--method="), "GET") {
				return true
			}
		case arg == "-f" || arg == "--raw-field" || arg == "-F" || arg == "--field" || arg == "--input":
			return true
		case strings.HasPrefix(arg, "-f") && len(arg) > 2:
			return true
		case strings.HasPrefix(arg, "-F") && len(arg) > 2:
			return true
		case strings.HasPrefix(arg, "--field=") || strings.HasPrefix(arg, "--raw-field=") || strings.HasPrefix(arg, "--input="):
			return true
		}
	}
	return false
}

func hasCommitMessage(args []string) bool {
	for _, arg := range args {
		if arg == "-m" || arg == "-F" || arg == "--message" || arg == "--file" || arg == "--no-edit" || strings.HasPrefix(arg, "--message=") || strings.HasPrefix(arg, "--file=") {
			return true
		}
		if strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && (strings.Contains(arg[1:], "m") || strings.Contains(arg[1:], "F")) {
			return true
		}
	}
	return false
}

func safeEditor(env []string) bool {
	for _, value := range env {
		if value == "GIT_EDITOR=true" || value == "GIT_EDITOR=:" {
			return true
		}
	}
	return false
}

func subcommand(args []string) (string, []string) {
	for i, arg := range args {
		if arg == "--" {
			if i+1 < len(args) {
				return args[i+1], args[i+2:]
			}
			return "", nil
		}
		if !strings.HasPrefix(arg, "-") {
			return arg, args[i+1:]
		}
	}
	return "", nil
}

func hasAny(args []string, names ...string) bool {
	for _, arg := range args {
		for _, name := range names {
			if arg == name || strings.HasPrefix(arg, name+"=") || (len(name) == 2 && strings.HasPrefix(arg, name) && len(arg) > 2) {
				return true
			}
		}
	}
	return false
}

func firstOperand(args []string) string {
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			return arg
		}
	}
	return ""
}

func mutatesWorkingTree(tool string, args []string) bool {
	command, rest := subcommand(args)
	if tool == "gh" {
		command, rest = ghSubcommand(args)
		return command == "pr" && firstOperand(rest) == "checkout"
	}
	switch command {
	case "am", "apply", "checkout", "cherry-pick", "clean", "commit", "merge", "mv", "pull", "push", "rebase", "reset", "restore", "revert", "rm", "sparse-checkout", "stash", "submodule", "switch":
		return true
	}
	return false
}

func rewriteMappedAbsoluteArgs(mapper *Mapper, projectID string, args []string) ([]string, error) {
	result := append([]string(nil), args...)
	for i, arg := range result {
		prefix := ""
		pathValue := arg
		if key, value, ok := strings.Cut(arg, "="); ok && filepath.IsAbs(value) {
			prefix = key + "="
			pathValue = value
		} else if strings.HasPrefix(arg, "-F") && len(arg) > 2 && filepath.IsAbs(arg[2:]) {
			prefix = "-F"
			pathValue = arg[2:]
		}
		if !filepath.IsAbs(pathValue) {
			continue
		}
		mapping, err := mapper.MapGuest(pathValue)
		if err != nil || mapping.Spec.ProjectID != projectID {
			return nil, fmt.Errorf("absolute path argument %q is outside the active workspace project", pathValue)
		}
		hostPath, err := mapper.ResolveHostPath(mapping)
		if err != nil {
			return nil, err
		}
		result[i] = prefix + hostPath
	}
	return result, nil
}

func commandEnvironment(overrides []string) []string {
	env := append([]string(nil), os.Environ()...)
	for _, value := range overrides {
		if value == "GIT_EDITOR=true" || value == "GIT_EDITOR=:" || value == "GIT_TERMINAL_PROMPT=0" || strings.HasPrefix(value, "GH_REPO=") {
			env = append(env, value)
		}
	}
	return env
}
