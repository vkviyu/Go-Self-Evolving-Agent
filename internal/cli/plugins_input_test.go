package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/config"
)

func TestPluginInputRejectsInvalidFilesBeforeOpeningRuntime(t *testing.T) {
	t.Setenv(config.HomeEnvironment, t.TempDir())
	for _, tc := range []struct{ name, raw, want string }{
		{"unknown field", `{"unexpected":true}`, `json: unknown field "unexpected"`},
		{"multiple values", `{} {}`, "plugin input must contain exactly one JSON value"},
		{"invalid candidate", `{}`, "invalid tool"},
		{"over size limit", strings.Repeat(" ", (256<<10)+1), "plugin input exceeds size limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "request.json")
			if err := os.WriteFile(path, []byte(tc.raw), 0600); err != nil {
				t.Fatal(err)
			}
			data := filepath.Join(t.TempDir(), "unopened")
			var out, log bytes.Buffer
			err := runPlugins(context.Background(), []string{"build", "--request", path, "--data-dir", data}, &out, &log)
			if err == nil || !strings.Contains(err.Error(), tc.want) || out.Len() != 0 {
				t.Fatalf("output=%s error=%v", out.String(), err)
			}
			if _, err := os.Stat(data); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid input opened runtime: %v", err)
			}
		})
	}
}

func TestBoundedPluginFileKeepsLimitAndRegularFileContract(t *testing.T) {
	dir := t.TempDir()
	if _, err := readBoundedPluginFile(dir, 10); err == nil || err.Error() != "plugin input must be a regular file" {
		t.Fatalf("directory error=%v", err)
	}
	path := filepath.Join(dir, "data")
	if err := os.WriteFile(path, []byte("12345"), 0600); err != nil {
		t.Fatal(err)
	}
	raw, err := readBoundedPluginFile(path, 5)
	if err != nil || string(raw) != "12345" {
		t.Fatalf("exact limit: raw=%s err=%v", raw, err)
	}
	if _, err := readBoundedPluginFile(path, 4); err == nil || err.Error() != "plugin input exceeds size limit" {
		t.Fatalf("overflow error=%v", err)
	}
}

func TestPluginInputForwardsValidatedCandidateAndCases(t *testing.T) {
	input, err := loadPluginInput(pluginOptions{action: "build", requestPath: "../../examples/evolution/echo.json"})
	if err != nil || input.request.Source == "" || len(input.request.Cases) == 0 {
		t.Fatalf("candidate=%+v err=%v", input.request, err)
	}
	path := filepath.Join(t.TempDir(), "cases.json")
	if err := os.WriteFile(path, []byte(`[{"name":"echo","input":{},"expected":{}}]`), 0600); err != nil {
		t.Fatal(err)
	}
	input, err = loadPluginInput(pluginOptions{action: "test", casesPath: path})
	if err != nil || len(input.cases) != 1 || input.cases[0].Name != "echo" {
		t.Fatalf("cases=%+v err=%v", input.cases, err)
	}
}
