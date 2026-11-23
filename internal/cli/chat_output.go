package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/render"
)

// turnOutput renders the streaming protocol used by pipes and automation.
// Interactive terminals are owned exclusively by internal/tui.
type turnOutput struct {
	writer           io.Writer
	renderer         *render.MarkdownStreamRenderer
	assistantStarted bool
	reasoningOpen    bool
	reasoningFilter  render.TextSanitizer
}

func newTurnOutput(writer io.Writer, color bool) *turnOutput {
	return &turnOutput{
		writer:   writer,
		renderer: render.NewMarkdownStreamRenderer(writer, color),
	}
}

func (output *turnOutput) Sink(_ context.Context, event agentruntime.Event) error {
	switch event.Type {
	case agentruntime.EventAssistantReasoningDelta:
		return output.writeReasoning(event.Delta)
	case agentruntime.EventAssistantTextDelta:
		return output.writeAssistant(event.Delta)
	default:
		return nil
	}
}

func (output *turnOutput) writeReasoning(delta string) error {
	delta = output.reasoningFilter.Filter(delta)
	if delta == "" {
		return nil
	}
	if !output.reasoningOpen {
		if output.assistantStarted {
			if _, err := io.WriteString(output.writer, "\n"); err != nil {
				return err
			}
		}
		output.reasoningOpen = true
	}
	_, err := io.WriteString(output.writer, delta)
	return err
}

func (output *turnOutput) writeAssistant(delta string) error {
	if delta == "" {
		return nil
	}
	if output.reasoningOpen {
		if _, err := fmt.Fprintln(output.writer); err != nil {
			return err
		}
		output.reasoningOpen = false
	}
	output.assistantStarted = true
	return output.renderer.Write(delta)
}

func (output *turnOutput) Finish(final *schema.Message) error {
	if output.reasoningOpen {
		if _, err := fmt.Fprintln(output.writer); err != nil {
			return err
		}
	}
	if !output.assistantStarted {
		if final != nil {
			if err := output.renderer.Write(final.Content); err != nil {
				return err
			}
		}
	}
	if err := output.renderer.Finish(); err != nil {
		return err
	}
	_, err := io.WriteString(output.writer, "\n")
	return err
}
