package evolutiontools

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/vkviyu/go-self-evolving-agent/internal/pluginbuild"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

func (adapter pluginAdapter) propose(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	invocation, ok := tool.InvocationFromContext(ctx)
	if !ok || invocation.SessionID == "" {
		return nil, errors.New("proposal requires trusted invocation scope")
	}
	var request pluginbuild.Request
	if err := decodeEvolutionArgs(args, &request); err != nil {
		return nil, err
	}
	request.OwnerSession = string(invocation.SessionID)
	result, err := adapter.host.BuildPlugin(ctx, request)
	if err != nil && (result.BuildID == "" || errors.Is(err, pluginbuild.ErrEvidence)) {
		return nil, err
	}
	return json.Marshal(result)
}

func (adapter pluginAdapter) inspect(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	request, result, err := adapter.scopedBuild(ctx, args)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"request": request, "result": result})
}

func (adapter pluginAdapter) publish(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	request, result, err := adapter.scopedBuild(ctx, args)
	if err != nil {
		return nil, err
	}
	if result == nil || result.State != "awaiting_approval" || result.TestReportID == "" {
		return nil, errors.New("candidate has no passing durable test report")
	}
	if _, err := adapter.host.PromotePlugin(ctx, request.Manifest.Tool.Name, request.Manifest.Tool.Revision, result.TestReportID, true); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"tool": request.Manifest.Tool.Name, "revision": request.Manifest.Tool.Revision, "available_next_turn": true})
}

func (adapter pluginAdapter) scopedBuild(ctx context.Context, args []byte) (pluginbuild.Request, *pluginbuild.Result, error) {
	var input struct {
		BuildID string `json:"build_id"`
	}
	if err := decodeEvolutionArgs(args, &input); err != nil {
		return pluginbuild.Request{}, nil, err
	}
	invocation, ok := tool.InvocationFromContext(ctx)
	if !ok || invocation.SessionID == "" {
		return pluginbuild.Request{}, nil, errors.New("build inspection requires trusted invocation scope")
	}
	request, result, err := adapter.host.PluginBuild(ctx, input.BuildID)
	if err != nil {
		return request, nil, err
	}
	if request.OwnerSession != string(invocation.SessionID) {
		return pluginbuild.Request{}, nil, errors.New("build belongs to a different session")
	}
	return request, result, nil
}
