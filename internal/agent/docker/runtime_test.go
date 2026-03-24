package docker_test

import (
	"testing"

	"github.com/ekovshilovsky/cloister/internal/agent"
	"github.com/ekovshilovsky/cloister/internal/agent/docker"
)

// Compile-time verification that DockerRuntime satisfies the agent.Runtime
// interface. This ensures that any future changes to the interface signature
// are caught at build time rather than at runtime.
var _ agent.Runtime = (*docker.DockerRuntime)(nil)

func TestDockerRuntimeImplementsRuntime(t *testing.T) {
	// Intentionally empty — the compile-time check above is the assertion.
	// This test function exists so that `go test` reports coverage of the
	// interface satisfaction invariant.
	_ = &docker.DockerRuntime{}
}
