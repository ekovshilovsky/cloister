package broker

import "strings"

// Extended attributes do not cross into a synchronized guest copy, but only
// some of them are worth telling anyone about.
//
// An attribute is immaterial when it describes the file's relationship to this
// Mac: where it came from, when it was last opened, which application the user
// granted access to. The guest copy is a different file on a different machine
// and has its own such relationship, so there is nothing there to lose.
//
// An attribute is material when it carries the file's content, or decides who
// may open it. Dropping one of those changes what the copy is rather than where
// it came from, and the user has to know.
//
// Anything unrecognized is material. The immaterial set below is finite,
// explicit and reasoned; everything outside it is reported. That is what keeps
// the classification safe as macOS adds attributes: a new one is noisy until
// somebody decides it is not, rather than silently dropped before anybody
// notices it exists.

// xattrRule matches one attribute by exact name or by prefix, and records why
// it is classified the way it is. The reason belongs next to the rule so the
// classification can be audited here, without reading the walk that applies it.
type xattrRule struct {
	name   string
	prefix string
	why    string
}

func (r xattrRule) matches(attribute string) bool {
	if r.prefix != "" {
		return strings.HasPrefix(attribute, r.prefix)
	}
	return attribute == r.name
}

// materialXattrRules are checked before the immaterial ones, so no later
// broadening of an immaterial prefix can swallow content or access semantics.
// They are redundant with the unrecognized-is-material default and exist to
// state the cases that must never become immaterial by accident.
var materialXattrRules = []xattrRule{
	{
		name: "com.apple.ResourceFork",
		why:  "the resource fork is file content, and a copy that silently loses it is corrupt rather than merely unlabelled",
	},
	{
		prefix: "system.posix_acl",
		why:    "an access control list decides who may open the file, so dropping it is a permission change",
	},
	{
		name: "com.apple.system.Security",
		why:  "the macOS access control list, with the same access consequences as a POSIX one",
	},
	{
		name: "com.apple.metadata:_kMDItemUserTags",
		why:  "Finder tags are labels the user deliberately attached to the file",
	},
	{
		name: "com.apple.metadata:kMDItemFinderComment",
		why:  "Finder comments are text the user deliberately attached to the file",
	},
	{
		prefix: "user.",
		why:    "the namespace tooling writes to deliberately, so its contents were put there by someone who wanted them",
	},
}

// immaterialXattrRules describe the file's relationship to this Mac. Each is
// stamped by the operating system rather than chosen by the user, and the guest
// copy forms its own equivalent where it needs one.
var immaterialXattrRules = []xattrRule{
	{
		name: "com.apple.provenance",
		why:  "records which installer or download put the file on this Mac, a fact about this Mac",
	},
	{
		name: "com.apple.quarantine",
		why:  "Gatekeeper's record of an untrusted origin; guest policy issues its own labels and deliberately does not import host ones",
	},
	{
		name: "com.apple.metadata:kMDItemWhereFroms",
		why:  "download origins describe how the file reached this Mac, not the synchronized copy's content",
	},
	{
		name: "com.apple.lastuseddate#PS",
		why:  "when this Mac last opened the file",
	},
	{
		name: "com.apple.macl",
		why:  "which application the user's click granted access to, meaningless to a machine with different applications",
	},
	{
		name: "com.apple.FinderInfo",
		why:  "Finder's per-file view state and legacy type codes, neither of which is content nor access",
	},
}

// isMaterialXattr reports whether an attribute is worth telling the user about.
func isMaterialXattr(attribute string) bool {
	return classifyXattr(attribute, materialXattrRules, immaterialXattrRules)
}

// classifyXattr applies the rules in precedence order. It takes them as
// arguments so the ordering guarantee can be tested against rules that overlap,
// which the shipped sets deliberately do not.
func classifyXattr(attribute string, material, immaterial []xattrRule) bool {
	for _, rule := range material {
		if rule.matches(attribute) {
			return true
		}
	}
	for _, rule := range immaterial {
		if rule.matches(attribute) {
			return false
		}
	}
	return true
}
