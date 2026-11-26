package config

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func parseATETimeout(value string) (time.Duration, error) {
	duration, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil || duration <= 0 || duration > 10*time.Minute {
		return 0, errors.New("ate.timeout must be positive and at most 10m")
	}
	return duration, nil
}

func applyATEEnvironment(target *ATEConfig) error {
	if value := strings.TrimSpace(os.Getenv("GSEA_ATE_STARTUP_TIMEOUT")); value != "" {
		duration, err := parseATEStartupTimeout(value)
		if err != nil {
			return errors.New("GSEA_ATE_STARTUP_TIMEOUT must be a positive duration")
		}
		target.StartupTimeout = duration
	}
	if value := strings.TrimSpace(os.Getenv("GSEA_ATE_ENABLED")); value != "" {
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			return errors.New("GSEA_ATE_ENABLED must be a boolean")
		}
		target.Enabled = enabled
	}
	for name, destination := range map[string]*string{
		"GSEA_ATE_BINARY": &target.Binary, "GSEA_ATE_DIRECTORY": &target.Directory,
	} {
		if value := os.Getenv(name); strings.TrimSpace(value) != "" {
			*destination = value
		}
	}
	if value := strings.TrimSpace(os.Getenv("GSEA_ATE_TIMEOUT")); value != "" {
		duration, err := parseATETimeout(value)
		if err != nil {
			return errors.New("GSEA_ATE_TIMEOUT must be positive and at most 10m")
		}
		target.Timeout = duration
	}
	return nil
}

func parseATEStartupTimeout(value string) (time.Duration, error) {
	d, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil || d <= 0 {
		return 0, errors.New("ate.startup_timeout must be a positive duration")
	}
	return d, nil
}

// ResolveATE resolves paths without creating the store: init-state needs an
// empty directory, and only the subprocess manager may initialize it under lock.
func (c Config) ResolveATE() (ATEConfig, error) {
	a := c.ATE
	if a.Binary == "" {
		a.Binary = "ate"
	}
	if a.Timeout == 0 {
		a.Timeout = 30 * time.Second
	}
	if a.Timeout < 0 || a.Timeout > 10*time.Minute {
		return ATEConfig{}, errors.New("ate.timeout must be positive and at most 10m")
	}
	if a.StartupTimeout < 0 {
		return ATEConfig{}, errors.New("ate.startup_timeout must be positive")
	}
	if a.Directory == "" {
		base := c.HomeDirectory
		if base == "" {
			base = c.DataDirectory
		}
		if base == "" {
			return ATEConfig{}, errors.New("ATE directory requires a home or data directory")
		}
		a.Directory = filepath.Join(base, "temporal")
	}
	for _, value := range []string{a.Binary, a.Directory} {
		if len(value) > 4096 || strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\x00\r\n") {
			return ATEConfig{}, errors.New("ATE paths must be nonblank, at most 4096 bytes, and contain no NUL or line breaks")
		}
	}
	var err error
	a.Directory, err = filepath.Abs(a.Directory)
	if err != nil {
		return ATEConfig{}, errors.New("resolve ATE directory")
	}
	if a.Directory == filepath.Dir(a.Directory) || len(a.Directory) > 4096 {
		return ATEConfig{}, errors.New("ATE directory must be a bounded non-root path")
	}
	// A bare name is resolved using PATH at launch. Explicit relative paths are
	// anchored now so later working directory changes cannot alter the binary.
	if strings.ContainsAny(a.Binary, `/\`) {
		a.Binary, err = filepath.Abs(a.Binary)
		if err != nil || len(a.Binary) > 4096 {
			return ATEConfig{}, errors.New("resolve ATE binary")
		}
	}
	return a, nil
}
