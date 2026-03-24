package vm

import "fmt"

// ResolveBackendName normalizes and validates a backend name string.
// Returns "colima" for empty strings to preserve backward compatibility
// with profiles created before the backend field was introduced.
func ResolveBackendName(name string) (string, error) {
	switch name {
	case "", "colima":
		return "colima", nil
	case "lume":
		return "lume", nil
	default:
		return "", fmt.Errorf("unknown backend %q (supported: colima, lume)", name)
	}
}
