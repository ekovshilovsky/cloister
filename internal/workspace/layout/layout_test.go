package layout

import (
	"strings"
	"testing"

	"cloister.io/internal/config"
)

func TestGuestRelPathMirrorWithoutOrgPrefix(t *testing.T) {
	got, err := GuestRelPath(Attributes{Selector: "apps/api", Org: "acme"}, config.Layout{Scheme: config.LayoutSchemeMirror}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got != "apps/api" {
		t.Fatalf("GuestRelPath() = %q, want apps/api", got)
	}
}

func TestGuestRelPathMirrorPrefixesOrgWhenGrouping(t *testing.T) {
	got, err := GuestRelPath(Attributes{Selector: "apps/api", Org: "acme"}, config.Layout{Scheme: config.LayoutSchemeMirror}, true)
	if err != nil {
		t.Fatal(err)
	}
	if got != "acme/apps/api" {
		t.Fatalf("GuestRelPath() = %q, want acme/apps/api", got)
	}
}

func TestGuestRelPathDoesNotPrefixEmptyOrg(t *testing.T) {
	got, err := GuestRelPath(Attributes{Selector: "tools/cli", Org: ""}, config.Layout{Scheme: config.LayoutSchemeMirror}, true)
	if err != nil {
		t.Fatal(err)
	}
	if got != "tools/cli" {
		t.Fatalf("GuestRelPath() = %q, want tools/cli", got)
	}
}

func TestGuestRelPathRejectsUnsafeSelectors(t *testing.T) {
	tests := []struct {
		name  string
		attrs Attributes
	}{
		{name: "empty", attrs: Attributes{}},
		{name: "absolute", attrs: Attributes{Selector: "/apps/api"}},
		{name: "dot dot", attrs: Attributes{Selector: "../escape"}},
		{name: "dot", attrs: Attributes{Selector: "."}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := GuestRelPath(test.attrs, config.Layout{Scheme: config.LayoutSchemeMirror}, false)
			if err == nil {
				t.Fatal("GuestRelPath() error = nil, want rejection")
			}
		})
	}
}

func TestGuestRelPathRejectsUnsupportedScheme(t *testing.T) {
	_, err := GuestRelPath(Attributes{Selector: "apps/api"}, config.Layout{Scheme: config.LayoutSchemeFlat}, false)
	if err == nil || !strings.Contains(err.Error(), "mirror") {
		t.Fatalf("GuestRelPath() error = %v, want unsupported scheme", err)
	}
}

func TestEffectiveGroupByOrg(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		multiOrg bool
		want     bool
	}{
		{name: "true always on", value: config.LayoutGroupTrue, multiOrg: false, want: true},
		{name: "false always off", value: config.LayoutGroupFalse, multiOrg: true, want: false},
		{name: "auto single org", value: config.LayoutGroupAuto, multiOrg: false, want: false},
		{name: "auto multi org", value: config.LayoutGroupAuto, multiOrg: true, want: true},
		{name: "empty auto single org", value: "", multiOrg: false, want: false},
		{name: "empty auto multi org", value: "", multiOrg: true, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := EffectiveGroupByOrg(config.Layout{GroupByOrg: test.value}, test.multiOrg)
			if got != test.want {
				t.Fatalf("EffectiveGroupByOrg(%q, %v) = %v, want %v", test.value, test.multiOrg, got, test.want)
			}
		})
	}
}

func TestMultiOrgIgnoresEmptyAndRequiresDistinct(t *testing.T) {
	if MultiOrg(nil) || MultiOrg([]string{"", ""}) || MultiOrg([]string{"acme", "", "acme"}) {
		t.Fatal("single-org and empty sets must not enable grouping")
	}
	if !MultiOrg([]string{"acme", "other"}) || !MultiOrg([]string{"", "acme", "other"}) {
		t.Fatal("distinct non-empty orgs must enable grouping")
	}
}
