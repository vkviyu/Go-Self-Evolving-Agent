package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/sandbox"
)

type publishedDiagnostics struct {
	diagnostics []protocolDiagnostic
}

type diagnosticWaiter struct {
	version int
	done    chan struct{}
}

type client struct {
	root          string
	stdin         io.WriteCloser
	stdout        *bufio.Reader
	processCancel context.CancelFunc
	waitProcess   chan error

	writeMu sync.Mutex
	nextID  atomic.Uint64

	pendingMu sync.Mutex
	pending   map[string]chan wireMessage

	diagnosticsMu sync.RWMutex
	diagnostics   map[string]publishedDiagnostics
	waiters       map[string][]diagnosticWaiter

	syncMu    sync.Mutex
	documents map[string]int

	done     chan struct{}
	stopOnce sync.Once
	stopMu   sync.Mutex
	stopErr  error
}

func startClient(ctx context.Context, root string, config Config) (*client, error) {
	processContext, processCancel := context.WithCancel(context.Background())
	executable, err := exec.LookPath(config.Command)
	if err != nil {
		processCancel()
		return nil, fmt.Errorf("resolve language server: %w", err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		processCancel()
		return nil, err
	}
	runner, err := sandbox.New(root)
	if err != nil {
		processCancel()
		return nil, fmt.Errorf("language server sandbox: %w", err)
	}
	stdinReader, stdin := io.Pipe()
	stdout, stdoutWriter := io.Pipe()
	closeProcess := func() {
		processCancel()
		_ = stdinReader.Close()
		_ = stdout.Close()
	}

	c := &client{
		root:          root,
		stdin:         stdin,
		stdout:        bufio.NewReader(stdout),
		processCancel: closeProcess,
		waitProcess:   make(chan error, 1),
		pending:       make(map[string]chan wireMessage),
		diagnostics:   make(map[string]publishedDiagnostics),
		waiters:       make(map[string][]diagnosticWaiter),
		documents:     make(map[string]int),
		done:          make(chan struct{}),
	}
	go c.readLoop()
	go func() {
		arguments := append([]string{executable}, config.Args...)
		for index, value := range arguments {
			arguments[index] = "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
		}
		environment := []string{"GOTOOLCHAIN=local", "GOENV=off", "GOWORK=off", "GOPROXY=off", "GOSUMDB=off", "PATH=" + filepath.Dir(executable) + string(os.PathListSeparator) + os.Getenv("PATH")}
		environment = append(environment, os.Environ()...)
		err := runner.Run(processContext, sandbox.Request{Command: "exec " + strings.Join(arguments, " "), WorkingDirectory: root, WorkspaceRoot: root, ReadOnly: true, Environment: environment, Stdin: stdinReader}, stdoutWriter, io.Discard)
		_ = stdoutWriter.CloseWithError(err)
		_ = stdinReader.CloseWithError(err)
		c.stop(err)
		c.waitProcess <- err
		close(c.waitProcess)
	}()

	startupTimeout := 10 * time.Second
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < startupTimeout {
		startupTimeout = time.Until(deadline)
	}
	startupContext, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()
	var initialized json.RawMessage
	if err := c.request(startupContext, "initialize", map[string]any{
		"processId": os.Getpid(),
		"clientInfo": map[string]any{
			"name":    "go-self-evolving-agent",
			"version": "1",
		},
		"rootUri": pathToURI(root),
		"workspaceFolders": []map[string]string{{
			"uri":  pathToURI(root),
			"name": filepath.Base(root),
		}},
		"capabilities": map[string]any{
			"workspace": map[string]any{"workspaceFolders": true},
			"textDocument": map[string]any{
				"publishDiagnostics": map[string]any{"relatedInformation": true, "versionSupport": true},
			},
		},
	}, &initialized); err != nil {
		_ = c.forceClose()
		return nil, fmt.Errorf("initialize language server %s: %w", config.Command, err)
	}
	if err := c.notify("initialized", map[string]any{}); err != nil {
		_ = c.forceClose()
		return nil, fmt.Errorf("notify initialized language server %s: %w", config.Command, err)
	}
	return c, nil
}

func (c *client) request(ctx context.Context, method string, params any, result any) error {
	if c == nil {
		return errors.New("language server client is unavailable")
	}
	id := c.nextID.Add(1)
	rawID := json.RawMessage(strconv.FormatUint(id, 10))
	key := wireIDKey(rawID)
	response := make(chan wireMessage, 1)
	c.pendingMu.Lock()
	c.pending[key] = response
	c.pendingMu.Unlock()
	defer c.removePending(key)

	if err := c.send(wireMessage{JSONRPC: "2.0", ID: rawID, Method: method, Params: mustMarshal(params)}); err != nil {
		return err
	}
	select {
	case message := <-response:
		if message.Error != nil {
			return message.Error
		}
		if result == nil || len(message.Result) == 0 || string(message.Result) == "null" {
			return nil
		}
		if err := json.Unmarshal(message.Result, result); err != nil {
			return fmt.Errorf("decode LSP %s response: %w", method, err)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return c.stoppedError()
	}
}

func (c *client) notify(method string, params any) error {
	return c.send(wireMessage{JSONRPC: "2.0", Method: method, Params: mustMarshal(params)})
}

func (c *client) send(message wireMessage) error {
	select {
	case <-c.done:
		return c.stoppedError()
	default:
	}
	// A server that stops reading stdin must not indefinitely block a tool
	// or shutdown. Closing the pipe also releases the protocol writer.
	timer := time.AfterFunc(2*time.Second, func() {
		c.processCancel()
		_ = c.stdin.Close()
		c.stop(errors.New("language server write timed out"))
	})
	defer timer.Stop()
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return writeProtocolMessage(c.stdin, message)
}

func (c *client) removePending(key string) {
	c.pendingMu.Lock()
	delete(c.pending, key)
	c.pendingMu.Unlock()
}

func (c *client) readLoop() {
	for {
		payload, err := readProtocolMessage(c.stdout)
		if err != nil {
			c.stop(err)
			return
		}
		var message wireMessage
		if err := json.Unmarshal(payload, &message); err != nil {
			c.stop(fmt.Errorf("decode LSP message: %w", err))
			return
		}
		switch {
		case message.Method == "textDocument/publishDiagnostics":
			c.publishDiagnostics(message.Params)
		case message.Method != "" && len(message.ID) > 0:
			c.handleServerRequest(message)
		case message.Method == "" && len(message.ID) > 0:
			c.deliverResponse(message)
		}
	}
}

func (c *client) deliverResponse(message wireMessage) {
	c.pendingMu.Lock()
	response := c.pending[wireIDKey(message.ID)]
	c.pendingMu.Unlock()
	if response == nil {
		return
	}
	select {
	case response <- message:
	default:
	}
}

func (c *client) handleServerRequest(message wireMessage) {
	response := wireMessage{JSONRPC: "2.0", ID: append(json.RawMessage(nil), message.ID...)}
	switch message.Method {
	case "workspace/configuration":
		var params struct {
			Items []json.RawMessage `json:"items"`
		}
		_ = json.Unmarshal(message.Params, &params)
		values := make([]any, len(params.Items))
		response.Result = mustMarshal(values)
	case "workspace/workspaceFolders":
		response.Result = mustMarshal([]map[string]string{{"uri": pathToURI(c.root), "name": filepath.Base(c.root)}})
	case "workspace/applyEdit":
		response.Result = mustMarshal(map[string]any{"applied": false, "failureReason": "client does not apply language-server edits"})
	case "client/registerCapability", "client/unregisterCapability", "window/workDoneProgress/create":
		response.Result = json.RawMessage("null")
	default:
		response.Error = &wireError{Code: -32601, Message: "method not supported by diagnostics client: " + message.Method}
	}
	_ = c.send(response)
}

func (c *client) publishDiagnostics(raw json.RawMessage) {
	var params publishDiagnosticsParams
	if err := json.Unmarshal(raw, &params); err != nil || params.URI == "" {
		return
	}
	copyDiagnostics := append([]protocolDiagnostic(nil), params.Diagnostics...)
	c.diagnosticsMu.Lock()
	c.diagnostics[params.URI] = publishedDiagnostics{diagnostics: copyDiagnostics}
	waiters := c.waiters[params.URI]
	remaining := waiters[:0]
	ready := make([]chan struct{}, 0, len(waiters))
	for _, waiter := range waiters {
		if params.Version == nil || *params.Version >= waiter.version {
			ready = append(ready, waiter.done)
		} else {
			remaining = append(remaining, waiter)
		}
	}
	if len(remaining) == 0 {
		delete(c.waiters, params.URI)
	} else {
		c.waiters[params.URI] = remaining
	}
	c.diagnosticsMu.Unlock()
	for _, waiter := range ready {
		close(waiter)
	}
}

func (c *client) syncDocument(ctx context.Context, path string, content []byte) error {
	c.syncMu.Lock()
	defer c.syncMu.Unlock()
	uri := pathToURI(path)
	version, opened := c.documents[uri]
	version++
	c.documents[uri] = version
	waiter := diagnosticWaiter{version: version, done: make(chan struct{})}
	c.diagnosticsMu.Lock()
	c.waiters[uri] = append(c.waiters[uri], waiter)
	c.diagnosticsMu.Unlock()
	removeWaiter := true
	defer func() {
		if removeWaiter {
			c.removeWaiter(uri, waiter.done)
		}
	}()
	var err error
	if !opened {
		err = c.notify("textDocument/didOpen", map[string]any{
			"textDocument": map[string]any{
				"uri": uri, "languageId": languageID(path), "version": version, "text": string(content),
			},
		})
	} else {
		err = c.notify("textDocument/didChange", map[string]any{
			"textDocument":   map[string]any{"uri": uri, "version": version},
			"contentChanges": []map[string]string{{"text": string(content)}},
		})
	}
	if err != nil {
		return err
	}
	select {
	case <-waiter.done:
		removeWaiter = false
		settle := time.NewTimer(100 * time.Millisecond)
		defer settle.Stop()
		select {
		case <-settle.C:
			return nil
		case <-ctx.Done():
			// A matching publication was received; context expiry during the
			// short settle window does not make that feedback stale.
			return nil
		case <-c.done:
			return c.stoppedError()
		}
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return c.stoppedError()
	}
}

func (c *client) deleteDocument(path string) error {
	c.syncMu.Lock()
	defer c.syncMu.Unlock()
	uri := pathToURI(path)
	_, opened := c.documents[uri]
	delete(c.documents, uri)
	if opened {
		if err := c.notify("textDocument/didClose", map[string]any{"textDocument": map[string]string{"uri": uri}}); err != nil {
			return err
		}
	}
	c.diagnosticsMu.Lock()
	delete(c.diagnostics, uri)
	delete(c.waiters, uri)
	c.diagnosticsMu.Unlock()
	return c.notify("workspace/didChangeWatchedFiles", map[string]any{
		"changes": []map[string]any{{"uri": uri, "type": 3}},
	})
}

func (c *client) removeWaiter(uri string, target chan struct{}) {
	c.diagnosticsMu.Lock()
	waiters := c.waiters[uri]
	for index, waiter := range waiters {
		if waiter.done == target {
			waiters = append(waiters[:index], waiters[index+1:]...)
			break
		}
	}
	if len(waiters) == 0 {
		delete(c.waiters, uri)
	} else {
		c.waiters[uri] = waiters
	}
	c.diagnosticsMu.Unlock()
}

func (c *client) diagnosticsSnapshot() map[string]publishedDiagnostics {
	c.diagnosticsMu.RLock()
	defer c.diagnosticsMu.RUnlock()
	result := make(map[string]publishedDiagnostics, len(c.diagnostics))
	for uri, published := range c.diagnostics {
		published.diagnostics = append([]protocolDiagnostic(nil), published.diagnostics...)
		result[uri] = published
	}
	return result
}

func (c *client) alive() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

func (c *client) stop(cause error) {
	c.stopOnce.Do(func() {
		if cause != nil {
			c.stopMu.Lock()
			c.stopErr = cause
			c.stopMu.Unlock()
		}
		close(c.done)
		c.pendingMu.Lock()
		for key := range c.pending {
			delete(c.pending, key)
		}
		c.pendingMu.Unlock()
	})
}

func (c *client) stoppedError() error {
	c.stopMu.Lock()
	defer c.stopMu.Unlock()
	if c.stopErr == nil || errors.Is(c.stopErr, io.EOF) {
		return errors.New("language server stopped")
	}
	return fmt.Errorf("language server stopped: %w", c.stopErr)
}

func (c *client) Close() error {
	if c == nil {
		return nil
	}
	if c.alive() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = c.request(shutdownContext, "shutdown", nil, nil)
		cancel()
		_ = c.notify("exit", nil)
		_ = c.stdin.Close()
	}
	select {
	case err := <-c.waitProcess:
		c.processCancel()
		return normalizeProcessExit(err)
	case <-time.After(2 * time.Second):
		return c.forceClose()
	}
}

func (c *client) forceClose() error {
	c.processCancel()
	_ = c.stdin.Close()
	select {
	case err := <-c.waitProcess:
		return normalizeProcessExit(err)
	case <-time.After(2 * time.Second):
		return errors.New("language server did not stop")
	}
}

func normalizeProcessExit(err error) error {
	if err == nil || errors.Is(err, context.Canceled) {
		return nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) && exitError.ProcessState != nil && exitError.ProcessState.ExitCode() == 0 {
		return nil
	}
	return err
}

func mustMarshal(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return encoded
}
