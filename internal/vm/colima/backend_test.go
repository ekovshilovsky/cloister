package colima_test

import (
	"github.com/ekovshilovsky/cloister/internal/vm"
	"github.com/ekovshilovsky/cloister/internal/vm/colima"
)

// Compile-time assertion that *Backend satisfies the vm.Backend interface.
// If any required method is missing or has an incorrect signature, this
// assignment will fail at compile time with a clear diagnostic.
var _ vm.Backend = (*colima.Backend)(nil)
