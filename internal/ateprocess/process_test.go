package ateprocess

import (
	"errors"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

func TestResolveBinary(t *testing.T) {
	root := t.TempDir()
	current := filepath.Join(root, "paired build", "local-agent")
	adjacent := filepath.Join(filepath.Dir(current), "ate")
	pathATE := filepath.Join(root, "path bin", "ate")
	explicit := filepath.Join(root, "explicit bin", "ate")
	for _, test := range []struct {
		name            string
		binary          string
		executableError bool
		available       map[string]string
		want            string
		wantLookups     []string
		wantExecutable  bool
		wantError       error
	}{
		{name: "explicit path unchanged", binary: explicit, want: explicit},
		{name: "adjacent without PATH", binary: "ate", available: map[string]string{adjacent: adjacent},
			want: adjacent, wantLookups: []string{adjacent}, wantExecutable: true},
		{name: "adjacent preferred over PATH", binary: "ate", available: map[string]string{adjacent: adjacent, "ate": pathATE},
			want: adjacent, wantLookups: []string{adjacent}, wantExecutable: true},
		{name: "PATH when adjacent unavailable", binary: "ate", available: map[string]string{"ate": pathATE},
			want: pathATE, wantLookups: []string{adjacent, "ate"}, wantExecutable: true},
		{name: "PATH when executable unavailable", binary: "ate", executableError: true, available: map[string]string{"ate": pathATE},
			want: pathATE, wantLookups: []string{"ate"}, wantExecutable: true},
		{name: "custom name uses PATH only", binary: "custom-ate", available: map[string]string{"custom-ate": pathATE},
			want: pathATE, wantLookups: []string{"custom-ate"}},
		{name: "missing binary", binary: "ate", wantLookups: []string{adjacent, "ate"}, wantExecutable: true, wantError: exec.ErrNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			var lookups []string
			executableCalled := false
			got, err := resolveBinary(test.binary, func() (string, error) {
				executableCalled = true
				if test.executableError {
					return "", errors.New("executable unavailable")
				}
				return current, nil
			}, func(name string) (string, error) {
				lookups = append(lookups, name)
				if path, ok := test.available[name]; ok {
					return path, nil
				}
				return "", exec.ErrNotFound
			})
			if got != test.want || !errors.Is(err, test.wantError) {
				t.Fatalf("resolveBinary = %q, %v; want %q, %v", got, err, test.want, test.wantError)
			}
			if !reflect.DeepEqual(lookups, test.wantLookups) || executableCalled != test.wantExecutable {
				t.Fatalf("unexpected search: lookups=%q executable=%v", lookups, executableCalled)
			}
		})
	}
}
