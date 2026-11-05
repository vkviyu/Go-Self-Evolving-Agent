package app

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

const diagnosticsToolName = "runtime_diagnostics"

func diagnosticsBinding(resolved config.Config) tool.Binding {
	return tool.Binding{
		Descriptor: tool.Descriptor{
			Name:        diagnosticsToolName,
			Description: "Reports local runtime versions and state paths without modifying the workspace.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
			Revision:    "builtin-v1",
			RiskClass:   tool.RiskPure,
		},
		Executor: tool.InvokeFunc(func(context.Context, json.RawMessage) (json.RawMessage, error) {
			return json.Marshal(map[string]string{
				"go_version":          runtime.Version(),
				"platform":            runtime.GOOS + "/" + runtime.GOARCH,
				"eino_version":        dependencyVersion("github.com/cloudwego/eino"),
				"eino_openai_version": dependencyVersion("github.com/cloudwego/eino-ext/components/model/openai"),
				"eino_ollama_version": dependencyVersion("github.com/cloudwego/eino-ext/components/model/ollama"),
				"wazero_version":      dependencyVersion("github.com/tetratelabs/wazero"),
				"session_database":    resolved.SessionDatabasePath(),
			})
		}),
	}
}

func dependencyVersion(modulePath string) string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	for _, dependency := range info.Deps {
		if dependency.Path == modulePath {
			if dependency.Replace != nil {
				dependency = dependency.Replace
			}
			return dependency.Version
		}
	}
	return "unknown"
}

type (
	// DoctorReport is stable machine-readable startup diagnostics.
	DoctorReport struct {
		Status             string              `json:"status"`
		SessionID          session.ID          `json:"session_id"`
		DatabasePath       string              `json:"database_path"`
		RegistryGeneration uint64              `json:"registry_generation"`
		EventTypes         []session.EventType `json:"event_types"`
		Runtime            json.RawMessage     `json:"runtime"`
		Coordination       CoordinationReport  `json:"coordination"`
	}

	// CoordinationReport describes the workspace coordination domain shared
	// with other instances, and whether instances of another GSEA_HOME are
	// excluded from the workspace.
	CoordinationReport struct {
		WorkspaceID        string `json:"workspace_id"`
		Directory          string `json:"directory"`
		InstanceID         string `json:"instance_id"`
		CrossHomeExclusion bool   `json:"cross_home_exclusion"`
		Degraded           string `json:"degraded,omitempty"`
	}
)

// Doctor exercises SQLite, the default-deny policy, registry snapshot and
// unified tool pipeline without contacting a model provider.
func (a *App) Doctor(ctx context.Context) (DoctorReport, error) {
	if a == nil {
		return DoctorReport{}, fmt.Errorf("application is unavailable")
	}
	a.stateMu.Lock()
	if a.closed {
		a.stateMu.Unlock()
		return DoctorReport{}, fmt.Errorf("application is closed")
	}
	registry := a.registry
	eventsStore := a.events
	pipeline := a.pipeline
	databasePath := a.config.SessionDatabasePath()
	coordination := a.domain.Status()
	a.stateMu.Unlock()
	if registry == nil || eventsStore == nil || pipeline == nil {
		return DoctorReport{}, fmt.Errorf("application diagnostics are unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	sessionID := session.ID("doctor-" + strconv.FormatInt(time.Now().UTC().UnixNano(), 36))
	snapshot := registry.Snapshot()
	runtimeSnapshotID := fmt.Sprintf("registry-%d", snapshot.Generation())
	if _, err := eventsStore.Append(ctx, sessionID, 0, session.PendingEvent{
		Type:              session.EventSessionCreated,
		RuntimeSnapshotID: runtimeSnapshotID,
		Payload: session.MustJSONPayload(map[string]any{
			"source": "doctor",
		}),
	}); err != nil {
		return DoctorReport{}, fmt.Errorf("create doctor session: %w", err)
	}
	result, err := pipeline.Execute(ctx, tool.Call{
		SessionID:         sessionID,
		RuntimeSnapshotID: runtimeSnapshotID,
		ToolName:          diagnosticsToolName,
		Arguments:         json.RawMessage(`{}`),
		Registry:          snapshot,
	})
	if err != nil {
		return DoctorReport{}, err
	}
	events, err := eventsStore.Load(ctx, sessionID, 0, 100)
	if err != nil {
		return DoctorReport{}, fmt.Errorf("load doctor events: %w", err)
	}
	eventTypes := make([]session.EventType, len(events))
	for index := range events {
		eventTypes[index] = events[index].Type
	}
	return DoctorReport{
		Status:             "ok",
		SessionID:          sessionID,
		DatabasePath:       databasePath,
		RegistryGeneration: snapshot.Generation(),
		EventTypes:         eventTypes,
		Runtime:            result.Data,
		Coordination: CoordinationReport{
			WorkspaceID: coordination.Identity.ID, Directory: coordination.Directory, InstanceID: coordination.InstanceID,
			CrossHomeExclusion: coordination.CrossHomeExclusion, Degraded: coordination.Degraded,
		},
	}, nil
}
