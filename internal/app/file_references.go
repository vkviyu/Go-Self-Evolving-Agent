package app

import (
	"context"
	"encoding/json"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/codingtools"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

func workspaceFileReferenceInspector(workspace *codingtools.Workspace, nativeScope string) func(context.Context, *tool.Pipeline, tool.Call, string) ([]agentruntime.FileReference, error) {
	var nativeRead tool.SnapshotEntry
	for _, binding := range scopeNativeBindings(nativeScope, workspace.Bindings()) {
		if binding.Descriptor.Name == "workspace_read" {
			nativeRead = tool.SnapshotEntry{Descriptor: binding.Descriptor, Implementation: binding.ImplementationID}
			break
		}
	}
	return func(ctx context.Context, pipeline *tool.Pipeline, call tool.Call, query string) ([]agentruntime.FileReference, error) {
		if call.ToolName != nativeRead.Descriptor.Name {
			return nil, nil
		}
		ctx = orchestration.WithActorScope(ctx, call.Actor)
		metadata, err := workspace.InspectFileReferences(ctx, query, func(path string) bool {
			read := call
			read.Arguments, _ = json.Marshal(struct {
				Path string `json:"path"`
			}{Path: path})
			return pipeline.AllowsReadInspection(ctx, read, nativeRead)
		})
		if err != nil || len(metadata) == 0 {
			return nil, err
		}
		references := make([]agentruntime.FileReference, 0, len(metadata))
		for _, file := range metadata {
			reference := agentruntime.FileReference{
				Path: file.Path, SizeBytes: file.SizeBytes, LineCount: file.LineCount,
				CharacterCount: file.CharacterCount, HeadingsTruncated: file.HeadingsTruncated,
			}
			for _, heading := range file.Headings {
				reference.Headings = append(reference.Headings, agentruntime.FileHeading(heading))
			}
			references = append(references, reference)
		}
		return references, nil
	}
}
