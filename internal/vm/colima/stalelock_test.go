package colima

import (
	"reflect"
	"testing"
)

func TestContainsStaleLockMarker(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{
			name: "in use by instance marker",
			in:   `{"level":"fatal","msg":"failed to run attach disk \"colima-cloister-work\", in use by instance \"colima-cloister-work\""}`,
			want: true,
		},
		{
			name: "attach disk marker only",
			in:   "level=fatal msg=\"failed to run attach disk x\"",
			want: true,
		},
		{
			name: "unrelated failure",
			in:   "level=fatal msg=\"error starting vm: context deadline exceeded\"",
			want: false,
		},
		{
			name: "empty",
			in:   "",
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := containsStaleLockMarker(tc.in); got != tc.want {
				t.Fatalf("containsStaleLockMarker(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestParsePIDList(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []int
	}{
		{name: "single", in: "7488\n", want: []int{7488}},
		{name: "multiple", in: "7488\n9120\n", want: []int{7488, 9120}},
		{name: "empty", in: "", want: nil},
		{name: "whitespace only", in: "  \n ", want: nil},
		{name: "ignores non-numeric", in: "7488\nabc\n12", want: []int{7488, 12}},
		{name: "ignores zero and negative", in: "0\n-3\n55", want: []int{55}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parsePIDList(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parsePIDList(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestNeedsRecovery(t *testing.T) {
	cases := []struct {
		name           string
		hostagentAlive bool
		holderCount    int
		registryLocked bool
		want           bool
	}{
		{name: "running VM is never touched", hostagentAlive: true, holderCount: 1, registryLocked: true, want: false},
		{name: "process orphan, no hostagent", hostagentAlive: false, holderCount: 1, registryLocked: false, want: true},
		{name: "lima registry lock only, no hostagent", hostagentAlive: false, holderCount: 0, registryLocked: true, want: true},
		{name: "both signals, no hostagent", hostagentAlive: false, holderCount: 2, registryLocked: true, want: true},
		{name: "nothing stale", hostagentAlive: false, holderCount: 0, registryLocked: false, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := needsRecovery(tc.hostagentAlive, tc.holderCount, tc.registryLocked); got != tc.want {
				t.Fatalf("needsRecovery(%v, %d, %v) = %v, want %v",
					tc.hostagentAlive, tc.holderCount, tc.registryLocked, got, tc.want)
			}
		})
	}
}

func TestPsHasHostagentFor(t *testing.T) {
	const work = `/opt/homebrew/bin/limactl hostagent --pidfile /Users/u/.colima/_lima/colima-cloister-work/ha.pid --socket /Users/u/.colima/_lima/colima-cloister-work/ha.sock --guestagent /opt/homebrew/share/lima/lima-guestagent.Linux-aarch64.gz colima-cloister-work
/opt/homebrew/bin/limactl usernet -p /Users/u/.colima/_lima/_networks/user-v2/usernet_user-v2.pid
/System/Library/Frameworks/Virtualization.framework/Versions/A/XPCServices/com.apple.Virtualization.VirtualMachine.xpc/Contents/MacOS/com.apple.Virtualization.VirtualMachine`

	cases := []struct {
		name     string
		psOut    string
		instance string
		want     bool
	}{
		{name: "hostagent present for instance", psOut: work, instance: "colima-cloister-work", want: true},
		{name: "no hostagent for other instance", psOut: work, instance: "colima-cloister-innolumi", want: false},
		{
			name:     "prefix collision does not false-positive",
			psOut:    work,
			instance: "colima-cloister-wor", // substring of the real instance
			want:     false,
		},
		{name: "usernet line is not a hostagent", psOut: work, instance: "user-v2", want: false},
		{name: "no processes", psOut: "", instance: "colima-cloister-work", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := psHasHostagentFor(tc.psOut, tc.instance); got != tc.want {
				t.Fatalf("psHasHostagentFor(_, %q) = %v, want %v", tc.instance, got, tc.want)
			}
		})
	}
}
