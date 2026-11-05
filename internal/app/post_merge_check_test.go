package app

import (
	"context"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/config"
)

func TestMergeExecutorUsesTheConfiguredPostMergeCheck(t *testing.T) {
	ctx := context.Background()
	for _, command := range []string{"", "go test ./..."} {
		cfg := config.Config{DataDirectory: t.TempDir(), WorkspaceDirectory: t.TempDir()}
		cfg.Agents.PostMergeCheck, cfg.Agents.PostMergeCheckTimeout = command, time.Minute
		a, err := Open(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		executor, err := a.NewMergeExecutor(nil, nil)
		if closeErr := a.Close(); err != nil || closeErr != nil {
			t.Fatal(err, closeErr)
		}
		check, ok := executor.Verifier.(postMergeCheck)
		if command == "" && executor.Verifier != nil {
			t.Fatalf("verifier without a configured check: %#v", executor.Verifier)
		}
		if command != "" && (!ok || check.command != command || check.timeout != time.Minute) {
			t.Fatalf("verifier = %#v, want check %q", executor.Verifier, command)
		}
	}
}
