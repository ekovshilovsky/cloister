// Proprietary and confidential. All rights reserved.

package vcsbroker

import (
	"context"
	"errors"
	"fmt"
	"io"
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

	guestCWD, args, err := effectiveGuestCommand(p.Mapper, request)
	if err != nil {
		return 2, err
	}
	mapping, err := p.Mapper.MapGuest(guestCWD)
	if err != nil {
		return 125, err
	}
	if err := validateCommand(request.Tool, args, request.Env); err != nil {
		return 2, err
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
	args, err = rewriteMappedAbsoluteArgs(p.Mapper, mapping.Spec.ProjectID, args)
	if err != nil {
		return 2, err
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

func effectiveGuestCommand(mapper *Mapper, request Request) (string, []string, error) {
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
		if arg == "--git-dir" || arg == "--work-tree" || arg == "--config-env" || arg == "-c" || strings.HasPrefix(arg, "--git-dir=") || strings.HasPrefix(arg, "--work-tree=") || strings.HasPrefix(arg, "--config-env=") || (strings.HasPrefix(arg, "-c") && !strings.HasPrefix(arg, "--")) {
			return "", nil, fmt.Errorf("git option %q is not available through the host VCS broker", arg)
		}
		if !strings.HasPrefix(arg, "-") || arg == "--" {
			break
		}
	}
	if _, err := mapper.MapGuest(cwd); err != nil {
		return "", nil, err
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

func validateCommand(tool string, args, env []string) error {
	command, rest := subcommand(args)
	if command == "" {
		return nil
	}
	if tool == "git" {
		if !allowedGitCommands[command] {
			return fmt.Errorf("git subcommand %q is not allowed through the host VCS broker", command)
		}
		if command == "commit" && !hasCommitMessage(rest) && !safeEditor(env) {
			return fmt.Errorf("interactive commit editors are unavailable through the VCS broker; pass -m, -F, or --no-edit")
		}
		if (command == "add" || command == "checkout" || command == "reset" || command == "stash") && hasAny(rest, "-p", "--patch", "-i", "--interactive") {
			return fmt.Errorf("interactive patch selection is unavailable through the VCS broker; run this command on the host")
		}
		if command == "rebase" && hasAny(rest, "-i", "--interactive") {
			return fmt.Errorf("interactive rebase is unavailable through the VCS broker; run it on the host")
		}
		if command == "rebase" && hasAny(rest, "-x", "--exec") {
			return fmt.Errorf("git rebase --exec is unavailable through the host VCS broker")
		}
		if command == "submodule" && firstOperand(rest) == "foreach" {
			return fmt.Errorf("git submodule foreach is unavailable through the host VCS broker")
		}
		if command == "bisect" && firstOperand(rest) == "run" {
			return fmt.Errorf("git bisect run is unavailable through the host VCS broker")
		}
		if command == "worktree" && firstOperand(rest) != "list" {
			return fmt.Errorf("mutating git worktree commands are unavailable through the VCS broker; create worktrees on the host")
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
				return fmt.Errorf("path argument %q escapes the mapped workspace project", arg)
			}
			if arg == "--no-index" || arg == "--unsafe-paths" || strings.HasPrefix(arg, "--open-files-in-pager") {
				return fmt.Errorf("git option %q is unavailable through the host VCS broker", arg)
			}
		}
		return nil
	}
	if command == "alias" || command == "extension" {
		return fmt.Errorf("gh %s is unavailable through the host VCS broker", command)
	}
	allowedGH := map[string]bool{
		"api": true, "auth": true, "issue": true, "pr": true, "release": true,
		"repo": true, "run": true, "search": true, "status": true, "workflow": true,
	}
	if !allowedGH[command] {
		return fmt.Errorf("gh subcommand %q is not allowed through the host VCS broker", command)
	}
	if command == "auth" && firstOperand(rest) != "status" {
		return fmt.Errorf("only gh auth status is available through the host VCS broker")
	}
	if command == "repo" && firstOperand(rest) != "view" && firstOperand(rest) != "list" {
		return fmt.Errorf("only gh repo view and gh repo list are available through the host VCS broker")
	}
	for _, arg := range rest {
		pathValue := arg
		if _, value, ok := strings.Cut(arg, "="); ok {
			pathValue = value
		}
		clean := filepath.Clean(pathValue)
		if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || strings.Contains(pathValue, "/../") {
			return fmt.Errorf("path argument %q escapes the mapped workspace project", arg)
		}
	}
	return nil
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
		if value == "GIT_EDITOR=true" || value == "GIT_EDITOR=:" || value == "GIT_TERMINAL_PROMPT=0" {
			env = append(env, value)
		}
	}
	return env
}
