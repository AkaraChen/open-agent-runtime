package builder

import (
	"archive/tar"
	"strings"
	"testing"

	"github.com/AkaraChen/open-agent-runtime/internal/manifest"
	"github.com/AkaraChen/open-agent-runtime/internal/spec"
)

func testBase() *BaseManifest {
	return &BaseManifest{Entries: map[string]FSEntry{
		"bin":                           {Type: tar.TypeSymlink, Link: "usr/bin"},
		"lib":                           {Type: tar.TypeSymlink, Link: "usr/lib"},
		"usr":                           {Type: tar.TypeDir},
		"usr/bin":                       {Type: tar.TypeDir},
		"usr/bin/perl":                  {Type: tar.TypeReg},
		"usr/lib":                       {Type: tar.TypeDir},
		"usr/lib/ld-linux-aarch64.so.1": {Type: tar.TypeSymlink, Link: "aarch64-linux-gnu/ld-linux-aarch64.so.1"},
		"usr/lib/aarch64-linux-gnu":     {Type: tar.TypeDir},
		"usr/lib/aarch64-linux-gnu/ld-linux-aarch64.so.1": {Type: tar.TypeReg},
		"usr/lib/aarch64-linux-gnu/libc.so.6":             {Type: tar.TypeReg},
		"etc":                                             {Type: tar.TypeDir},
		"etc/hostname":                                    {Type: tar.TypeReg},
		"opt":                                             {Type: tar.TypeDir},
	}}
}

var arm64 = spec.Platform{OS: "linux", Arch: "arm64"}

func comp(name string, files ...string) manifest.Component {
	return manifest.Component{Name: name, Kind: "binary", Files: files}
}

func TestResolveIn(t *testing.T) {
	b := testBase()
	if _, ok := resolveIn(b.Entries, "/lib/ld-linux-aarch64.so.1"); !ok {
		t.Fatal("interpreter via /lib symlink not resolved")
	}
	if _, ok := resolveIn(b.Entries, "/bin/perl"); !ok {
		t.Fatal("/bin/perl not resolved")
	}
	if _, ok := resolveIn(b.Entries, "/lib/missing.so"); ok {
		t.Fatal("missing file resolved")
	}
}

func TestConflicts(t *testing.T) {
	cases := []struct {
		name string
		pl   manifest.PlatformLock
		want string
	}{
		{"ok", manifest.PlatformLock{Components: []manifest.Component{comp("a", "opt/", "opt/a"), comp("b", "opt/", "opt/b")}}, ""},
		{"comp-comp", manifest.PlatformLock{Components: []manifest.Component{comp("a", "opt/x"), comp("b", "opt/x")}}, "provided by both a and b"},
		{"comp-base", manifest.PlatformLock{Components: []manifest.Component{comp("a", "etc/", "etc/hostname")}}, "already exists in the base"},
		{"symlink-ancestor", manifest.PlatformLock{Components: []manifest.Component{comp("a", "bin/", "bin/tool")}}, "shadow base symlink"},
		{"reserved", manifest.PlatformLock{Components: []manifest.Component{comp("a", "nix/", "nix/store/x")}}, "reserved path"},
		{"bins", manifest.PlatformLock{Packages: []manifest.Package{{Attr: "a", Bins: []string{"x"}}, {Attr: "b", Bins: []string{"x"}}}}, "PATH conflict"},
		{"env", manifest.PlatformLock{Components: []manifest.Component{
			{Name: "a", Env: map[string]string{"K": "1"}}, {Name: "b", Env: map[string]string{"K": "2"}}}}, "env conflict"},
		{"abi-arch", manifest.PlatformLock{Components: []manifest.Component{{Name: "a", Files: []string{"opt/a"},
			ELF: []manifest.ELFInfo{{Path: "/opt/a", Machine: "amd64", Static: true}}}}}, "built for amd64"},
		{"abi-interp", manifest.PlatformLock{Components: []manifest.Component{{Name: "a", Files: []string{"opt/a"},
			ELF: []manifest.ELFInfo{{Path: "/opt/a", Machine: "arm64", Interpreter: "/lib/ld-musl-aarch64.so.1"}}}}}, "does not provide"},
		{"abi-ok", manifest.PlatformLock{Components: []manifest.Component{{Name: "a", Files: []string{"opt/a"},
			ELF: []manifest.ELFInfo{{Path: "/opt/a", Machine: "arm64", Interpreter: "/lib/ld-linux-aarch64.so.1", Needed: []string{"libc.so.6"}}}}}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := checkConflicts(&tc.pl, testBase(), nil, arm64)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}

func TestOverrideAllowsBaseReplacement(t *testing.T) {
	c := comp("a", "etc/", "etc/hostname")
	c.Override = true
	pl := manifest.PlatformLock{Components: []manifest.Component{c}}
	if _, err := checkConflicts(&pl, testBase(), nil, arm64); err != nil {
		t.Fatal(err)
	}
}

func TestShadowWarning(t *testing.T) {
	pl := manifest.PlatformLock{Packages: []manifest.Package{{Attr: "perl", Bins: []string{"perl"}}}}
	w, err := checkConflicts(&pl, testBase(), nil, arm64)
	if err != nil || len(w) != 1 {
		t.Fatalf("warnings %v err %v", w, err)
	}
}
