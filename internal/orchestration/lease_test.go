package orchestration

import "testing"

func TestValidLeasePath(t *testing.T) {
	t.Parallel()

	for path, want := range map[string]bool{
		WorkspaceRootLeasePath: true,
		"src/main.go":          true,
		"./src":                true,
		`src\main.go`:          true,
		"":                     false,
		".":                    false,
		"./":                   false,
		"../outside":           false,
		"src/../../outside":    false,
		`..\outside`:           false,
		"/etc/passwd":          false,
		"a\x00b":               false,
	} {
		if got := validLeasePath(path); got != want {
			t.Errorf("validLeasePath(%q) = %v, want %v", path, got, want)
		}
	}
}
