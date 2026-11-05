// Package toolbridge adapts an evaluation harness's Unix-socket tools to the
// normal GSEA registry and Tool Pipeline. Eino still owns the model/tool loop;
// the bridge only transports individual invocations to the harness process.
package toolbridge

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

const (
	ManifestSchema = "gsea.tool-bridge.manifest.v1"
	CallSchema     = "gsea.tool-bridge.call.v1"
	ResultSchema   = "gsea.tool-bridge.result.v1"
	maxManifest    = 4 << 20
	maxResult      = 32 << 20
)

type Manifest struct {
	Schema string       `json:"schema"`
	Tools  []Definition `json:"tools"`
}

type Definition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type callRequest struct {
	Schema    string          `json:"schema"`
	CallID    string          `json:"call_id"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
}

type callResult struct {
	Schema string          `json:"schema"`
	CallID string          `json:"call_id"`
	Result json.RawMessage `json:"result"`
	Error  string          `json:"error,omitempty"`
}

type executor struct {
	socketPath string
	toolName   string
}

// Load validates a manifest and builds execute-risk bindings. The caller must
// explicitly add policy and approval rules for the returned names.
func Load(manifestPath, socketPath string) ([]tool.Binding, error) {
	manifestPath = strings.TrimSpace(manifestPath)
	socketPath = strings.TrimSpace(socketPath)
	if manifestPath == "" || socketPath == "" {
		return nil, errors.New("tool bridge requires manifest and socket paths")
	}
	if !filepath.IsAbs(socketPath) {
		return nil, errors.New("tool bridge socket path must be absolute")
	}
	file, err := os.Open(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("read tool bridge manifest: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("tool bridge manifest must be a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxManifest+1))
	if err != nil {
		return nil, fmt.Errorf("read tool bridge manifest: %w", err)
	}
	if len(data) > maxManifest {
		return nil, fmt.Errorf("tool bridge manifest exceeds %d bytes", maxManifest)
	}
	var manifest Manifest
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("decode tool bridge manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("tool bridge manifest must contain exactly one JSON value")
	}
	if manifest.Schema != ManifestSchema {
		return nil, fmt.Errorf("unsupported tool bridge manifest schema %q", manifest.Schema)
	}
	if len(manifest.Tools) == 0 {
		return nil, errors.New("tool bridge manifest must contain at least one tool")
	}
	digest := sha256.Sum256(data)
	revision := "bridge-v1-" + hex.EncodeToString(digest[:6])
	bindings := make([]tool.Binding, 0, len(manifest.Tools))
	for _, definition := range manifest.Tools {
		binding := tool.Binding{
			Descriptor: tool.Descriptor{
				Name:         definition.Name,
				Description:  definition.Description,
				InputSchema:  append(json.RawMessage(nil), definition.InputSchema...),
				Revision:     revision,
				RiskClass:    tool.RiskExecute,
				Capabilities: []string{"tool.invoke"},
			},
			Executor: &executor{socketPath: socketPath, toolName: definition.Name},
		}
		if err := binding.Descriptor.Validate(); err != nil {
			return nil, fmt.Errorf("validate bridged tool %q: %w", definition.Name, err)
		}
		bindings = append(bindings, binding)
	}
	return bindings, nil
}

func (e *executor) Invoke(ctx context.Context, arguments json.RawMessage) (json.RawMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	dialer := net.Dialer{Timeout: 5 * time.Second}
	connection, err := dialer.DialContext(ctx, "unix", e.socketPath)
	if err != nil {
		return nil, fmt.Errorf("connect tool bridge for %s: %w", e.toolName, err)
	}
	defer connection.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	} else {
		_ = connection.SetDeadline(time.Now().Add(5 * time.Minute))
	}
	request := callRequest{
		Schema: CallSchema, CallID: orchestration.ToolCallIDFromContext(ctx),
		Tool: e.toolName, Arguments: append(json.RawMessage(nil), arguments...),
	}
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		return nil, fmt.Errorf("send bridged tool call: %w", err)
	}
	line, err := bufio.NewReader(io.LimitReader(connection, maxResult+1)).ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("receive bridged tool result: %w", err)
	}
	if len(line) > maxResult {
		return nil, fmt.Errorf("bridged tool result exceeds %d bytes", maxResult)
	}
	var response callResult
	if err := json.Unmarshal(line, &response); err != nil {
		return nil, fmt.Errorf("decode bridged tool result: %w", err)
	}
	if response.Schema != ResultSchema || response.CallID != request.CallID {
		return nil, errors.New("bridged tool result does not match the request")
	}
	if len(response.Result) == 0 {
		response.Result = json.RawMessage("null")
	}
	if response.Error == "" {
		return response.Result, nil
	}
	encoded, err := json.Marshal(map[string]any{
		"error":  response.Error,
		"result": json.RawMessage(response.Result),
	})
	if err != nil {
		return nil, err
	}
	return encoded, nil
}
