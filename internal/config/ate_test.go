package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestATEConfigurationDefaultsAndPrecedence(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	t.Setenv(HomeEnvironment, home)
	t.Setenv(ProfileEnvironment, "")
	for _, name := range []string{"GSEA_ATE_ENABLED", "GSEA_ATE_BINARY", "GSEA_ATE_DIRECTORY", "GSEA_ATE_TIMEOUT"} {
		t.Setenv(name, "")
	}
	loaded, err := Load(LoadOptions{WorkingDirectory: project, Strict: true})
	want := ATEConfig{Enabled: true, Binary: "ate", Directory: filepath.Join(home, "temporal"), Timeout: 30 * time.Second}
	if err != nil || loaded.ATE != want {
		t.Fatalf("defaults=%+v err=%v", loaded.ATE, err)
	}
	writeConfig(t, filepath.Join(home, configFileName), "[ate]\nenabled=false\nbinary='./bin/custom ate'\ndirectory='./custom-store'\ntimeout='2s'\n")
	loaded, err = Load(LoadOptions{WorkingDirectory: project, Strict: true})
	binary, _ := filepath.Abs("./bin/custom ate")
	directory, _ := filepath.Abs("./custom-store")
	if err != nil || loaded.ATE != (ATEConfig{Binary: binary, Directory: directory, Timeout: 2 * time.Second}) {
		t.Fatalf("file=%+v err=%v", loaded.ATE, err)
	}
	t.Setenv("GSEA_ATE_ENABLED", "true")
	t.Setenv("GSEA_ATE_BINARY", "ate-env")
	t.Setenv("GSEA_ATE_DIRECTORY", filepath.Join(home, "env-store"))
	t.Setenv("GSEA_ATE_TIMEOUT", "4s")
	loaded, err = Load(LoadOptions{WorkingDirectory: project, Strict: true})
	if err != nil || !loaded.ATE.Enabled || loaded.ATE.Binary != "ate-env" || loaded.ATE.Timeout != 4*time.Second || loaded.ATE.Directory != filepath.Join(home, "env-store") {
		t.Fatalf("environment=%+v err=%v", loaded.ATE, err)
	}
	for _, path := range []string{want.Directory, directory, loaded.ATE.Directory} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("configuration created store %s: %v", path, err)
		}
	}
}

func TestATEProgrammaticConfigAndSharedHome(t *testing.T) {
	home := t.TempDir()
	for _, withHome := range []bool{false, true} {
		data := filepath.Join(home, "sessions", "one")
		c := Config{DataDirectory: data, WorkspaceDirectory: home}
		base := data
		if withHome {
			c.HomeDirectory, base = home, home
		}
		resolved, err := c.Resolve()
		if err != nil || resolved.ATE.Enabled || resolved.ATE.Binary != "ate" || resolved.ATE.Timeout != 30*time.Second || resolved.ATE.Directory != filepath.Join(base, "temporal") {
			t.Fatalf("resolved=%+v err=%v", resolved.ATE, err)
		}
		if _, err := os.Stat(resolved.ATE.Directory); !os.IsNotExist(err) {
			t.Fatalf("Resolve created temporal store: %v", err)
		}
	}
}

func TestATERejectsInvalidConfiguration(t *testing.T) {
	for _, a := range []ATEConfig{
		{Timeout: -time.Second}, {Timeout: 11 * time.Minute},
		{Binary: strings.Repeat("a", 4097)}, {Binary: "ate\x00bad"},
		{Directory: "/"}, {Directory: " \t"}, {Directory: "bad\npath"},
		{Directory: strings.Repeat("a", 4097)},
	} {
		if _, err := (Config{DataDirectory: t.TempDir(), ATE: a}).Resolve(); err == nil {
			t.Fatal("invalid ATE config accepted")
		}
	}
	t.Setenv(HomeEnvironment, t.TempDir())
	t.Setenv(ProfileEnvironment, "")
	for _, name := range []string{"GSEA_ATE_ENABLED", "GSEA_ATE_BINARY", "GSEA_ATE_DIRECTORY", "GSEA_ATE_TIMEOUT"} {
		t.Setenv(name, "")
	}
	for name, value := range map[string]string{"GSEA_ATE_ENABLED": "maybe", "GSEA_ATE_TIMEOUT": "0s"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, value)
			if _, err := Load(LoadOptions{WorkingDirectory: t.TempDir()}); err == nil {
				t.Fatal("invalid ATE environment accepted")
			}
		})
	}
	for _, value := range []string{"0s", "-1s", "11m", "soon"} {
		writeConfig(t, filepath.Join(os.Getenv(HomeEnvironment), configFileName), "[ate]\ntimeout='"+value+"'\n")
		if _, err := Load(LoadOptions{WorkingDirectory: t.TempDir()}); err == nil {
			t.Fatalf("invalid TOML timeout %q accepted", value)
		}
	}
}
