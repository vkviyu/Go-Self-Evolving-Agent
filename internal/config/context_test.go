package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestContextAutoDefaultsAndOverrides(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	t.Setenv(HomeEnvironment, home)
	t.Setenv(ProfileEnvironment, "")
	t.Setenv("GSEA_CONTEXT_WINDOW_TOKENS", "")
	t.Setenv("GSEA_COMPACTION_TRIGGER_TOKENS", "")
	t.Setenv("GSEA_MAX_OUTPUT_TOKENS", "")
	loaded, err := Load(LoadOptions{WorkingDirectory: project})
	if err != nil || loaded.Agent.ContextWindowTokens != 0 || loaded.Agent.CompactionTriggerTokens != 0 || loaded.Agent.MaxOutputTokens != 0 {
		t.Fatalf("auto defaults=%+v err=%v", loaded.Agent, err)
	}
	writeConfig(t, filepath.Join(home, configFileName), "[agent]\ncontext_window_tokens = 64000\ncompaction_trigger_tokens = 48000\nmax_output_tokens = 6144\n")
	loaded, err = Load(LoadOptions{WorkingDirectory: project})
	if err != nil || loaded.Agent.ContextWindowTokens != 64000 || loaded.Agent.CompactionTriggerTokens != 48000 || loaded.Agent.MaxOutputTokens != 6144 {
		t.Fatalf("file overrides=%+v err=%v", loaded.Agent, err)
	}
	t.Setenv("GSEA_CONTEXT_WINDOW_TOKENS", "0")
	t.Setenv("GSEA_COMPACTION_TRIGGER_TOKENS", "0")
	t.Setenv("GSEA_MAX_OUTPUT_TOKENS", "0")
	loaded, err = Load(LoadOptions{WorkingDirectory: project})
	if err != nil || loaded.Agent.ContextWindowTokens != 0 || loaded.Agent.CompactionTriggerTokens != 0 || loaded.Agent.MaxOutputTokens != 0 {
		t.Fatalf("environment auto reset=%+v err=%v", loaded.Agent, err)
	}
	t.Setenv("GSEA_CONTEXT_WINDOW_TOKENS", "-1")
	if _, err := Load(LoadOptions{WorkingDirectory: project}); err == nil {
		t.Fatal("negative context window accepted")
	}
}

func TestMaxOutputConfigurationAllowsAutoAndRejectsNegative(t *testing.T) {
	for _, test := range []struct {
		name, file, environment string
		want                    int
		wantError               string
	}{
		{"automatic file", "0", "", 0, ""},
		{"explicit file", "2048", "", 2048, ""},
		{"negative file", "-1", "", 0, "agent.max_output_tokens"},
		{"automatic environment", "2048", "0", 0, ""},
		{"explicit environment", "0", "4096", 4096, ""},
		{"negative environment", "0", "-1", 0, "GSEA_MAX_OUTPUT_TOKENS"},
	} {
		t.Run(test.name, func(t *testing.T) {
			home, project := t.TempDir(), t.TempDir()
			t.Setenv(HomeEnvironment, home)
			t.Setenv(ProfileEnvironment, "")
			t.Setenv("GSEA_CONTEXT_WINDOW_TOKENS", "")
			t.Setenv("GSEA_COMPACTION_TRIGGER_TOKENS", "")
			t.Setenv("GSEA_MAX_OUTPUT_TOKENS", test.environment)
			writeConfig(t, filepath.Join(home, configFileName), "[agent]\nmax_output_tokens = "+test.file+"\n")
			loaded, err := Load(LoadOptions{WorkingDirectory: project})
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("expected %q, got %v", test.wantError, err)
				}
				return
			}
			if err != nil || loaded.Agent.MaxOutputTokens != test.want {
				t.Fatalf("output allowance=%d, want=%d err=%v", loaded.Agent.MaxOutputTokens, test.want, err)
			}
		})
	}
}
