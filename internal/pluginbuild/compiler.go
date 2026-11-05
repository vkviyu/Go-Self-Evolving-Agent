package pluginbuild

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/plugin"
	"github.com/vkviyu/go-self-evolving-agent/internal/sandbox"
	"github.com/vkviyu/go-self-evolving-agent/sdk/pluginabi"
)

type Compiler struct{}

func digest(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }

// Compile accepts source only, not a project directory. All files in the
// temporary build root are created from validated bytes and the embedded SDK.
func (Compiler) Compile(ctx context.Context, source string) (artifact []byte, evidence BuildEvidence, result error) {
	evidence.SourceSHA256 = digest([]byte(source))
	if err := ValidateSource(source); err != nil {
		return nil, evidence, err
	}
	buildCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	goRoot, err := filepath.EvalSymlinks(runtime.GOROOT())
	if err != nil {
		return nil, evidence, fmt.Errorf("resolve configured host toolchain: %w", err)
	}
	goPath := filepath.Join(goRoot, "bin", "go")
	env := []string{"PATH=" + filepath.Dir(goPath) + ":/usr/bin:/bin", "GOROOT=" + goRoot, "GOTOOLCHAIN=local", "GOENV=off", "GOWORK=off", "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0", "GOOS=wasip1", "GOARCH=wasm", "GOMAXPROCS=2", "GOMEMLIMIT=256MiB"}
	directory, err := os.MkdirTemp("", "gsea-candidate-")
	if err != nil {
		return nil, evidence, err
	}
	defer os.RemoveAll(directory)
	runner, err := sandbox.New(directory)
	if err != nil {
		return nil, evidence, err
	}
	evidence.Sandbox = runner.Name()
	evidence.ToolchainHashScope = toolchainHashScope
	evidence.MemoryLimitMode = "GOMEMLIMIT-soft-hint; no-hard-process-memory-limit"
	evidence.ToolchainSHA256, err = fingerprintToolchain(buildCtx, goRoot)
	if err != nil {
		return nil, evidence, fmt.Errorf("fingerprint configured host toolchain: %w", err)
	}
	var versionOutput, versionErrors boundedBuildLog
	err = runner.Run(buildCtx, sandbox.Request{
		Command: "exec " + shellQuote(goPath) + " version", WorkingDirectory: directory,
		WorkspaceRoot: directory, Environment: env,
	}, &versionOutput, &versionErrors)
	if err != nil {
		evidence.Log = versionErrors.String()
		return nil, evidence, fmt.Errorf("isolated compiler version probe failed: %w", err)
	}
	evidence.Toolchain = strings.TrimSpace(versionOutput.String())
	if !strings.HasPrefix(evidence.Toolchain, "go version go1.27.0 ") {
		return nil, evidence, fmt.Errorf("candidate compiler requires Go 1.27.0: %s", evidence.Toolchain)
	}
	if err := os.Mkdir(filepath.Join(directory, "pluginabi"), 0700); err != nil {
		return nil, evidence, err
	}
	sdkHash := sha256.New()
	for _, name := range []string{"abi.go", "bridge_wasip1.go"} {
		raw, err := pluginabi.SourceFiles.ReadFile(name)
		if err != nil {
			return nil, evidence, err
		}
		fmt.Fprintln(sdkHash, name)
		sdkHash.Write(raw)
		if err := os.WriteFile(filepath.Join(directory, "pluginabi", name), raw, 0600); err != nil {
			return nil, evidence, err
		}
	}
	evidence.SDKSHA256 = hex.EncodeToString(sdkHash.Sum(nil))
	files := map[string]string{
		"go.mod":       "module local-agent-candidate\n\ngo 1.27.0\n",
		"candidate.go": source,
		"main.go":      "package main\nimport \"local-agent-candidate/pluginabi\"\nfunc init(){ pluginabi.Register(Invoke) }\nfunc main(){}\n",
	}
	for name, value := range files {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(value), 0600); err != nil {
			return nil, evidence, err
		}
	}
	var log boundedBuildLog
	command := "ulimit -t 90 && ulimit -f 131072 && exec " + shellQuote(goPath) + " build -mod=readonly -trimpath -buildvcs=false -buildmode=c-shared -o candidate.wasm ."
	err = runner.Run(buildCtx, sandbox.Request{Command: command, WorkingDirectory: directory, WorkspaceRoot: directory, Environment: env}, &log, &log)
	evidence.Log = log.String()
	if err != nil {
		return nil, evidence, fmt.Errorf("isolated candidate compilation failed: %w", err)
	}
	if err := buildCtx.Err(); err != nil {
		return nil, evidence, err
	}
	afterFingerprint, err := fingerprintToolchain(buildCtx, goRoot)
	if err != nil || afterFingerprint != evidence.ToolchainSHA256 {
		return nil, evidence, fmt.Errorf("configured toolchain changed during compilation: %w", errors.Join(err, errToolchainChanged))
	}
	output, err := os.Open(filepath.Join(directory, "candidate.wasm"))
	if err != nil {
		return nil, evidence, err
	}
	defer output.Close()
	artifact, err = io.ReadAll(io.LimitReader(output, plugin.MaxArtifactBytes+1))
	if err != nil {
		return nil, evidence, err
	}
	if len(artifact) == 0 || len(artifact) > plugin.MaxArtifactBytes {
		return nil, evidence, fmt.Errorf("compiled artifact exceeds 32 MiB")
	}
	evidence.ArtifactSHA256, evidence.Success = digest(artifact), true
	return artifact, evidence, nil
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }

type boundedBuildLog struct {
	mu   sync.Mutex
	data []byte
}

func (log *boundedBuildLog) Write(value []byte) (int, error) {
	log.mu.Lock()
	defer log.mu.Unlock()
	length := len(value)
	remaining := (64 << 10) - len(log.data)
	if remaining > 0 {
		if len(value) > remaining {
			value = value[:remaining]
		}
		log.data = append(log.data, value...)
	}
	return length, nil
}
func (log *boundedBuildLog) String() string {
	log.mu.Lock()
	defer log.mu.Unlock()
	return string(log.data)
}
