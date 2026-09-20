package pantry

import "testing"

// The three spellings the pantry actually uses, each read from a real recipe:
// kernel.org/linux says `platforms: linux`, github.com/vmware/tdnf says
// `platforms: [linux]`, and x.org/xserver says
// `platforms: [linux/x86-64, linux/aarch64]`.
func TestSupportsAcceptsEverySpelling(t *testing.T) {
	for _, c := range []struct {
		name      string
		platforms any
		want      map[string]bool // "os/arch" -> supported
	}{
		{"absent", nil, map[string]bool{
			"darwin/aarch64": true, "linux/x86-64": true, "windows/x86-64": true,
		}},
		{"bare string", "linux", map[string]bool{
			"linux/x86-64": true, "linux/aarch64": true, "darwin/aarch64": false,
		}},
		{"list of os names", []any{"linux"}, map[string]bool{
			"linux/aarch64": true, "darwin/aarch64": false,
		}},
		{"os/arch slugs", []any{"linux/x86-64", "linux/aarch64"}, map[string]bool{
			"linux/x86-64": true, "linux/aarch64": true,
			"darwin/aarch64": false,
			// An arch the list does not name is not admitted, even on an OS it does.
			"linux/riscv64": false,
		}},
		{"darwin only", []any{"darwin"}, map[string]bool{
			"darwin/aarch64": true, "darwin/x86-64": true, "linux/x86-64": false,
		}},
		// A non-string entry cannot name a platform, so it admits nothing —
		// and the entries beside it still count.
		{"list with a non-string entry", []any{1, "linux"}, map[string]bool{
			"linux/x86-64": true, "darwin/aarch64": false,
		}},
		{"list of only non-strings", []any{1}, map[string]bool{
			"linux/x86-64": false, "darwin/aarch64": false,
		}},
		// A declaration shaped like nothing we know supports nothing rather
		// than everything: a recipe we cannot read is not a candidate.
		{"unreadable", map[string]any{"linux": true}, map[string]bool{
			"linux/x86-64": false, "darwin/aarch64": false,
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := &Recipe{Platforms: c.platforms}
			for slug, want := range c.want {
				os, arch := slug[:len(slug)-len(arch2(slug))-1], arch2(slug)
				if got := Supports(r, os, arch); got != want {
					t.Errorf("Supports(%q, %q) = %v, want %v", os, arch, got, want)
				}
			}
		})
	}
}

// arch2 is the part after the slash, so the table can be written as slugs.
func arch2(slug string) string {
	for i := len(slug) - 1; i >= 0; i-- {
		if slug[i] == '/' {
			return slug[i+1:]
		}
	}
	return ""
}

// A nil recipe is not a reason to refuse: the caller that has not parsed one
// yet should not be told the platform is unsupported.
func TestSupportsNilRecipe(t *testing.T) {
	if !Supports(nil, "darwin", "aarch64") {
		t.Error("a nil recipe should support every platform")
	}
}
