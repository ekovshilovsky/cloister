package broker

import "context"

// Operation names a broker lifecycle method for test observations.
type Operation string

const (
	OperationCreate    Operation = "create"
	OperationFlush     Operation = "flush"
	OperationPause     Operation = "pause"
	OperationResume    Operation = "resume"
	OperationTerminate Operation = "terminate"
	OperationStatus    Operation = "status"
)

// Call records one mock broker invocation.
type Call struct {
	Operation Operation
	Spec      SessionSpec
}

// Mock is a VM-free SyncBroker used by lifecycle and command tests.
type Mock struct {
	Calls       []Call
	Errors      map[Operation]error
	StatusValue Status
}

func (m *Mock) record(operation Operation, spec SessionSpec) error {
	m.Calls = append(m.Calls, Call{Operation: operation, Spec: spec})
	if m.Errors != nil {
		return m.Errors[operation]
	}
	return nil
}

func (m *Mock) Create(_ context.Context, spec SessionSpec) error {
	return m.record(OperationCreate, spec)
}

func (m *Mock) Flush(_ context.Context, spec SessionSpec) error {
	return m.record(OperationFlush, spec)
}

func (m *Mock) Pause(_ context.Context, spec SessionSpec) error {
	return m.record(OperationPause, spec)
}

func (m *Mock) Resume(_ context.Context, spec SessionSpec) error {
	return m.record(OperationResume, spec)
}

func (m *Mock) Terminate(_ context.Context, spec SessionSpec) error {
	return m.record(OperationTerminate, spec)
}

func (m *Mock) Status(_ context.Context, spec SessionSpec) (Status, error) {
	if err := m.record(OperationStatus, spec); err != nil {
		return Status{}, err
	}
	status := m.StatusValue
	if status.State == "" {
		status.State = StateActive
	}
	return status, nil
}
