package evaluation

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/sandbox"
)

const CapabilitiesSchema = "gsea.eval.capabilities.v1"

type Capability struct {
	Available bool   `json:"available"`
	Path      string `json:"path,omitempty"`
	Name      string `json:"name,omitempty"`
	Error     string `json:"error,omitempty"`
}

type Capabilities struct {
	Schema        string                `json:"schema"`
	OS            string                `json:"os"`
	Architecture  string                `json:"architecture"`
	Commands      map[string]Capability `json:"commands"`
	PythonModules map[string]Capability `json:"python_modules"`
	Sandbox       Capability            `json:"sandbox"`
	DockerDaemon  Capability            `json:"docker_daemon"`
}

func InspectCapabilities(ctx context.Context) Capabilities {
	if ctx == nil {
		ctx = context.Background()
	}
	report := Capabilities{
		Schema: CapabilitiesSchema, OS: runtime.GOOS, Architecture: runtime.GOARCH,
		Commands: make(map[string]Capability), PythonModules: make(map[string]Capability),
	}
	for _, name := range []string{"git", "docker", "python3", "go", "node", "npm", "java", "mvn", "rustc", "cargo", "gopls"} {
		report.Commands[name] = inspectCommand(name)
	}
	report.Sandbox = inspectSandbox(ctx)
	report.DockerDaemon = inspectDockerDaemon(ctx, report.Commands["docker"])
	python := report.Commands["python3"]
	for _, module := range []string{"swebench", "inspect_ai", "inspect_evals", "agentdojo", "aider", "terminal_bench"} {
		report.PythonModules[module] = inspectPythonModule(ctx, python, module)
	}
	return report
}

func inspectCommand(name string) Capability {
	path, err := exec.LookPath(name)
	if err != nil {
		return Capability{Error: err.Error()}
	}
	return Capability{Available: true, Path: path}
}

func inspectSandbox(ctx context.Context) Capability {
	directory, err := os.MkdirTemp("", "gsea-eval-sandbox-")
	if err != nil {
		return Capability{Error: err.Error()}
	}
	defer os.RemoveAll(directory)
	canonical, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return Capability{Error: err.Error()}
	}
	runner, err := sandbox.New(canonical)
	if err != nil {
		return Capability{Error: err.Error()}
	}
	probeContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var output bytes.Buffer
	err = runner.Run(probeContext, sandbox.Request{Command: "true", WorkingDirectory: canonical}, &output, &output)
	if err != nil {
		return Capability{Name: runner.Name(), Error: joinProbeError(err, output.String())}
	}
	return Capability{Available: true, Name: runner.Name()}
}

func inspectDockerDaemon(ctx context.Context, docker Capability) Capability {
	if !docker.Available {
		return Capability{Error: "docker CLI is unavailable"}
	}
	probeContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	command := exec.CommandContext(probeContext, docker.Path, "info", "--format", "{{.ServerVersion}}")
	var output cappedBuffer
	output.limit = 32 << 10
	command.Stdout, command.Stderr = &output, &output
	if err := command.Run(); err != nil {
		return Capability{Path: docker.Path, Error: joinProbeError(err, output.String())}
	}
	return Capability{Available: true, Path: docker.Path, Name: strings.TrimSpace(output.String())}
}

func inspectPythonModule(ctx context.Context, python Capability, module string) Capability {
	if !python.Available {
		return Capability{Error: "python3 is unavailable"}
	}
	probeContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	script := "import importlib.util,sys;sys.exit(0 if importlib.util.find_spec(sys.argv[1]) else 1)"
	command := exec.CommandContext(probeContext, python.Path, "-c", script, module)
	var output cappedBuffer
	output.limit = 32 << 10
	command.Stdout, command.Stderr = &output, &output
	if err := command.Run(); err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) && exitError.ExitCode() == 1 && strings.TrimSpace(output.String()) == "" {
			return Capability{Error: "module is not installed"}
		}
		return Capability{Error: joinProbeError(err, output.String())}
	}
	return Capability{Available: true}
}

func joinProbeError(err error, output string) string {
	message := err.Error()
	if output = strings.TrimSpace(output); output != "" {
		message = fmt.Sprintf("%s: %s", message, output)
	}
	return message
}
