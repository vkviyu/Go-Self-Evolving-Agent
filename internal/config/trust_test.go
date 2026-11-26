package config

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestProjectConfigurationRequiresExplicitTrust(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	t.Setenv(HomeEnvironment, home)
	t.Setenv(ProfileEnvironment, "")
	t.Setenv("GSEA_TRUST_PROJECT_CONFIG", "")
	t.Setenv("GSEA_AGENTS_POST_MERGE_CHECK", "")
	writeConfig(t, filepath.Join(project, projectConfigDirectory, configFileName), "[agents]\npost_merge_check = \"true\"\n")
	if _, err := Load(LoadOptions{WorkingDirectory: project}); !errors.Is(err, ErrProjectConfigUntrusted) {
		t.Fatalf("untrusted project accepted: %v", err)
	}
	loaded, err := Load(LoadOptions{WorkingDirectory: project, TrustProjectConfig: true})
	if err != nil || loaded.Agents.PostMergeCheck != "true" {
		t.Fatalf("explicit grant ignored: post_merge_check=%q err=%v", loaded.Agents.PostMergeCheck, err)
	}
}

func TestUserConfigurationIsNotMistakenForUntrustedAncestor(t *testing.T) {
	home := t.TempDir()
	t.Setenv(HomeEnvironment, filepath.Join(home, projectConfigDirectory))
	t.Setenv("GSEA_TRUST_PROJECT_CONFIG", "")
	writeConfig(t, filepath.Join(home, projectConfigDirectory, configFileName), "[agent]\nmax_iterations = 7\n")
	loaded, err := Load(LoadOptions{WorkingDirectory: home})
	if err != nil || loaded.Agent.MaxIterations != 7 {
		t.Fatalf("trusted user config rejected: %v", err)
	}
}
