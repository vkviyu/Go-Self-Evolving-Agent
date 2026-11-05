package config

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestProjectConfigurationRequiresExplicitTrust(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	t.Setenv(HomeEnvironment, home)
	t.Setenv("GSEA_TRUST_PROJECT_CONFIG", "")
	writeConfig(t, filepath.Join(project, projectConfigDirectory, configFileName), "[lsp]\ncommand = \"untrusted-command\"\n")
	if _, err := Load(LoadOptions{WorkingDirectory: project}); !errors.Is(err, ErrProjectConfigUntrusted) {
		t.Fatalf("untrusted project accepted: %v", err)
	}
	loaded, err := Load(LoadOptions{WorkingDirectory: project, TrustProjectConfig: true})
	if err != nil || loaded.LSP.Command != "untrusted-command" {
		t.Fatalf("explicit grant ignored: %+v %v", loaded.LSP, err)
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
