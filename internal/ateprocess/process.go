package ateprocess

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/vkviyu/agent-temporal-engine/transport"
)

const tokenEnvironment = "GSEA_ATE_PRIVATE_LAUNCH_TOKEN"

// ATE uses the server-side request timeout for idle stdio frame reads too.
// Keep the connection open across model/tool waits. Manager and ClientLease
// still apply the configured ATE timeout to each host operation.
const nativeConnectionTimeout = time.Hour

type (
	process struct {
		cmd     *exec.Cmd
		stream  transport.Stream
		done    chan struct{}
		stop    func() bool
		once    sync.Once
		waitErr error
		err     error
		grace   time.Duration
	}
)

// resolveBinary prefers the adjacent ate for the default name, keeping a local
// build pair together even when PATH contains a different version. Only the
// executable's directory and PATH are considered; workspace directories are
// never searched. Injected lookups keep selection independently testable.
func resolveBinary(binary string, executable func() (string, error), lookPath func(string) (string, error)) (string, error) {
	if strings.ContainsAny(binary, `/\`) {
		return binary, nil
	}
	if binary == "ate" {
		if current, err := executable(); err == nil && filepath.IsAbs(current) {
			if adjacent, err := lookPath(filepath.Join(filepath.Dir(current), "ate")); err == nil {
				return adjacent, nil
			}
		}
	}
	return lookPath(binary)
}

func (m *Manager) start(ctx context.Context, binary, token string) (*process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// The trusted host uses global snapshots to bind its bridge to this store.
	// ATE authorizes snapshots separately from AllSpaces and hello capabilities.
	cmd := exec.Command(binary, "serve", "--stdio", "--all-spaces", "--allow-global-snapshot",
		"--agent-profile", "--event-profile", "--principal", "gsea",
		"--token-env", tokenEnvironment, "--request-timeout", nativeConnectionTimeout.String(),
		m.options.Directory)
	// The credential is never a command argument or a process-wide variable.
	// Remove an inherited value before adding this launch's random credential.
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, tokenEnvironment+"=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, tokenEnvironment+"="+token)
	input, writer, err := os.Pipe()
	if err != nil {
		return nil, &batchError{stage: "stdin pipe creation", cause: err}
	}
	reader, output, err := os.Pipe()
	if err != nil {
		input.Close()
		writer.Close()
		return nil, &batchError{stage: "stdout pipe creation", cause: err}
	}
	// These pipes are owned independently of exec.Wait: Wait must never close
	// stdout while a protocol reader is still draining a response.
	cmd.Stdin, cmd.Stdout = input, output
	stream, err := transport.NewPipes(reader, writer)
	if err != nil {
		input.Close()
		output.Close()
		reader.Close()
		writer.Close()
		return nil, &batchError{stage: "transport creation", cause: err}
	}
	err = cmd.Start()
	input.Close()
	output.Close()
	if err != nil {
		stream.Close()
		return nil, &batchError{stage: "subprocess start", cause: err}
	}
	return m.watch(ctx, cmd, stream), nil
}

func (m *Manager) initProcess(ctx context.Context, binary string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cmd := exec.Command(binary, "init-state", "--agent-profile", "--event-profile", m.options.Directory)
	if err := cmd.Start(); err != nil {
		return &batchError{stage: "store initialization start", cause: err}
	}
	p := m.watch(ctx, cmd, nil)
	select {
	case <-p.done:
	case <-ctx.Done():
	}
	err := p.finish()
	p.stop()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return &batchError{stage: "store initialization", cause: err}
	}
	return nil
}

func (m *Manager) watch(ctx context.Context, cmd *exec.Cmd, stream transport.Stream) *process {
	p := &process{cmd: cmd, stream: stream, done: make(chan struct{}), grace: min(m.options.Timeout, time.Second)}
	// Nil Stderr sends output directly to the null device. No unbounded buffer,
	// output-copy goroutine, stderr content or credentials can reach an error.
	go func() {
		p.waitErr = cmd.Wait()
		close(p.done)
	}()
	p.stop = context.AfterFunc(ctx, func() { _ = p.finish() })
	return p
}

// finish first closes the native stream (EOF asks ATE to drain accepted work).
// It always joins Wait, including after Kill, before the caller releases its
// store lock. A failed kill never permits premature ownership release.
func (p *process) finish() error {
	p.once.Do(func() {
		if p.stream != nil {
			_ = p.stream.Close()
		} else {
			select {
			case <-p.done:
			default:
				_ = p.cmd.Process.Signal(os.Interrupt)
			}
		}
		timer := time.NewTimer(p.grace)
		defer timer.Stop()
		select {
		case <-p.done:
		case <-timer.C:
			_ = p.cmd.Process.Kill()
			<-p.done
		}
		if p.waitErr != nil {
			p.err = errors.New("ATE child did not exit successfully")
		}
	})
	return p.err
}
