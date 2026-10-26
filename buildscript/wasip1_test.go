package buildscript

import (
	"strings"
	"testing"

	"github.com/go-pkgx/bk/target"
)

func wasiTgt() target.Target {
	return target.Target{Platform: "wasip1", Arch: "wasm", Triple: "wasm32-wasip1"}
}

// TestWrapWasip1Cross: a Go-only wasip1 build adds no default llvm.org (no C
// compiler is wanted), no pkgx-libc sysroot, and no unix link flags.
func TestWrapWasip1Cross(t *testing.T) {
	for _, host := range []target.Target{linuxTgt(), {Platform: "linux", Arch: "aarch64"}} {
		s := Wrap(WrapOptions{
			UserScript: "go build", Deps: []string{"go.dev"},
			Target: wasiTgt(), Host: host, Home: "/h", SrcRoot: "/s", LibcPkgx: true,
		})
		if strings.Contains(s, `"+llvm.org"`) {
			t.Error("wasip1 target must not add default llvm.org")
		}
		if !strings.Contains(s, `"+go.dev"`) {
			t.Error("the recipe's own dependency must stay")
		}
		for _, bad := range []string{"LDFLAGS", "-pie", "rpath", "glibc", "linux-headers"} {
			if strings.Contains(s, bad) {
				t.Errorf("wasip1 target got %q:\n%s", bad, s)
			}
		}
	}
}
