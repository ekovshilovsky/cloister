package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// defaultHeadlessMounts is the curated set of mount names applied automatically
// when a profile runs headless and no explicit mounts policy has been configured.
// These represent the minimal set of directories required by AI toolchains
// (Claude plugins, skills, and agents) together with the primary code workspace.
var defaultHeadlessMounts = []string{
	"code",
	"claude-plugins",
	"claude-skills",
	"claude-agents",
}

// ResourcePolicy controls which named resources (tunnels or mounts) are
// permitted for a given profile. It supports three forms:
//
//   - Scalar "auto"  — allow all resources unconditionally.
//   - Scalar "none"  — deny all resources unconditionally.
//   - Sequence        — an explicit allowlist of resource names.
//
// When the field is omitted from the YAML document entirely, IsSet is false
// and the policy is resolved at runtime via ResolveForTunnels or
// ResolveForMounts according to the execution context.
type ResourcePolicy struct {
	// IsSet is false when the field was not present in the YAML document,
	// distinguishing an intentional "auto" from an absent configuration.
	IsSet bool

	// Mode holds the scalar form of the policy: "auto", "none", or ""
	// when the policy is expressed as an explicit Names list.
	Mode string

	// Names is the explicit allowlist of resource names. It is populated only
	// when Mode is "" and IsSet is true.
	Names []string
}

// UnmarshalYAML implements yaml.Unmarshaler. It handles both the scalar string
// form ("auto", "none") and the sequence form (an explicit list of names).
// Any other scalar value is rejected with a descriptive error.
func (p *ResourcePolicy) UnmarshalYAML(value *yaml.Node) error {
	p.IsSet = true

	switch value.Kind {
	case yaml.ScalarNode:
		switch value.Value {
		case "auto", "none":
			p.Mode = value.Value
			return nil
		default:
			return fmt.Errorf(
				"config: invalid resource policy %q: must be \"auto\", \"none\", or a list of names",
				value.Value,
			)
		}

	case yaml.SequenceNode:
		var names []string
		if err := value.Decode(&names); err != nil {
			return fmt.Errorf("config: failed to decode resource policy list: %w", err)
		}
		p.Names = names
		return nil

	default:
		return fmt.Errorf(
			"config: resource policy must be a scalar (\"auto\"/\"none\") or a list of names, got node kind %v",
			value.Kind,
		)
	}
}

// MarshalYAML implements yaml.Marshaler. It serialises the policy back to
// either a scalar string or a sequence. When IsSet is false — meaning the
// field was never configured — it returns nil so that the parent document omits
// the key entirely.
func (p ResourcePolicy) MarshalYAML() (interface{}, error) {
	if !p.IsSet {
		return nil, nil
	}
	if p.Mode != "" {
		return p.Mode, nil
	}
	return p.Names, nil
}

// IsAllowed reports whether the named resource is permitted under this policy.
// "auto" and an unset policy both allow everything; "none" denies everything;
// an explicit Names list allows only members of that list.
func (p ResourcePolicy) IsAllowed(name string) bool {
	if !p.IsSet || p.Mode == "auto" {
		return true
	}
	if p.Mode == "none" {
		return false
	}
	// List-based policy: check membership in the explicit allowlist.
	for _, n := range p.Names {
		if n == name {
			return true
		}
	}
	return false
}

// ResolveForTunnels returns the effective tunnel policy, applying environment-
// aware defaults when the policy has not been explicitly configured.
//
// Default resolution:
//   - Headless (non-interactive): "none" — tunnels require explicit consent in
//     automated environments to prevent unintended port exposure.
//   - Interactive: "auto" — the user is present to approve tunnel requests.
func (p ResourcePolicy) ResolveForTunnels(isHeadless bool) ResourcePolicy {
	if p.IsSet {
		return p
	}
	if isHeadless {
		return ResourcePolicy{IsSet: true, Mode: "none"}
	}
	return ResourcePolicy{IsSet: true, Mode: "auto"}
}

// ResolveForMounts returns the effective mount policy, applying environment-
// aware defaults when the policy has not been explicitly configured.
//
// Default resolution:
//   - Headless (non-interactive): the curated defaultHeadlessMounts allowlist —
//     only well-known AI toolchain directories are shared without explicit consent.
//   - Interactive: "auto" — the user is present to approve mount requests.
func (p ResourcePolicy) ResolveForMounts(isHeadless bool) ResourcePolicy {
	if p.IsSet {
		return p
	}
	if isHeadless {
		names := make([]string, len(defaultHeadlessMounts))
		copy(names, defaultHeadlessMounts)
		return ResourcePolicy{IsSet: true, Names: names}
	}
	return ResourcePolicy{IsSet: true, Mode: "auto"}
}
