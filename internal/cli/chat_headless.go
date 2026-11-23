package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/cloudwego/eino/schema"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/conversation"
	"github.com/vkviyu/go-self-evolving-agent/internal/render"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
)

type headlessConversation interface {
	ID() session.ID
	CompletedTurns() int
	RunTurn(context.Context, string, agentruntime.EventSink) (agentruntime.Result, error)
}

type recoveryConversation interface {
	Recoveries(context.Context) ([]conversation.Recovery, error)
	ResumeTurn(context.Context, string, agentruntime.EventSink) (agentruntime.Result, error)
}

func printRecoveries(ctx context.Context, writer io.Writer, chat recoveryConversation) error {
	recoveries, err := chat.Recoveries(ctx)
	if err != nil {
		return err
	}
	return writeRecoveries(writer, recoveries)
}

func writeRecoveries(writer io.Writer, recoveries []conversation.Recovery) error {
	if recoveries == nil {
		recoveries = []conversation.Recovery{}
	}
	return json.NewEncoder(writer).Encode(recoveries)
}

func runHeadlessResume(ctx context.Context, writer io.Writer, chat recoveryConversation, id, format string) error {
	return renderHeadlessOperation(ctx, writer, format, func(ctx context.Context, sink agentruntime.EventSink) (agentruntime.Result, error) {
		return chat.ResumeTurn(ctx, id, sink)
	})
}

// runHeadlessChat is the non-TTY protocol used by pipes and automation. It is
// deliberately separate from the Bubble Tea terminal application.
func runHeadlessChat(
	ctx context.Context,
	stdin io.Reader,
	stdout, stderr io.Writer,
	chat headlessConversation,
	options chatOptions,
) error {
	printHeadlessHeader(stderr, chat, options)
	if recovery, ok := chat.(recoveryConversation); ok {
		items, err := recovery.Recoveries(ctx)
		if err != nil {
			return err
		}
		if len(items) > 0 {
			fmt.Fprintln(stderr, "interrupted work; use /recoveries or /resume <turn-id> (not executed automatically)")
			if err := writeRecoveries(stderr, items); err != nil {
				return err
			}
		}
	}

	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	inputLines := scanInput(ctx, scanner)
	for {
		var scanned scanResult
		select {
		case <-ctx.Done():
			return ctx.Err()
		case value, ok := <-inputLines:
			if !ok {
				return ctx.Err()
			}
			scanned = value
		}
		if scanned.err != nil {
			return fmt.Errorf("read chat input: %w", scanned.err)
		}
		if scanned.done {
			return nil
		}
		input := strings.TrimSpace(scanned.line)
		if input == "" {
			continue
		}
		fields := strings.Fields(input)
		if fields[0] == "/recoveries" || fields[0] == "/resume" {
			recovery, ok := chat.(recoveryConversation)
			if !ok {
				return fmt.Errorf("recovery is unavailable")
			}
			if fields[0] == "/recoveries" && len(fields) == 1 {
				if err := printRecoveries(ctx, stdout, recovery); err != nil {
					return err
				}
			} else if fields[0] == "/resume" && len(fields) == 2 {
				if err := runHeadlessResume(ctx, stdout, recovery, fields[1], options.headlessFormat); err != nil {
					return err
				}
			} else {
				return fmt.Errorf("usage: /recoveries or /resume <turn-id>")
			}
			continue
		}
		switch input {
		case "/exit", "/quit":
			return nil
		case "/session":
			fmt.Fprintln(stderr, chat.ID())
			continue
		case "/help":
			fmt.Fprintln(stderr, "commands: /help, /session, /recoveries, /resume <turn-id>, /exit")
			continue
		}
		if err := runHeadlessTurnWithFormat(ctx, stdout, chat, input, options.headlessFormat); err != nil {
			return err
		}
	}
}

func runHeadlessPrompt(
	ctx context.Context,
	stdout, stderr io.Writer,
	chat headlessConversation,
	options chatOptions,
	prompt string,
) error {
	printHeadlessHeader(stderr, chat, options)
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return fmt.Errorf("evaluation prompt must not be empty")
	}
	return runHeadlessTurnWithFormat(ctx, stdout, chat, prompt, options.headlessFormat)
}

func printHeadlessHeader(stderr io.Writer, chat headlessConversation, options chatOptions) {
	fmt.Fprintf(stderr, "session: %s (loaded %d completed turns)\n", chat.ID(), chat.CompletedTurns())
	if options.usesResponses() {
		fmt.Fprintf(stderr, "model: %s (reasoning effort: %s, summary: auto)\n", options.model.Model, options.model.ReasoningEffort)
	}
}

func runHeadlessTurnWithFormat(ctx context.Context, stdout io.Writer, chat headlessConversation, input, format string) error {
	return renderHeadlessOperation(ctx, stdout, format, func(ctx context.Context, sink agentruntime.EventSink) (agentruntime.Result, error) {
		return chat.RunTurn(ctx, input, sink)
	})
}

func renderHeadlessOperation(ctx context.Context, stdout io.Writer, format string, run func(context.Context, agentruntime.EventSink) (agentruntime.Result, error)) error {
	if format == headlessFormatJSON {
		turnResult, err := run(ctx, func(context.Context, agentruntime.Event) error { return nil })
		if err != nil {
			return err
		}
		result := headlessJSONResult{Schema: "gsea.chat.result.v1"}
		if turnResult.Final != nil {
			result.Content = turnResult.Final.Content
			usage := tokenusage.FromMessage(turnResult.Final)
			result.Usage = headlessJSONUsage{
				Input: usage.Input, Cached: usage.Cached, Output: usage.Output,
				Reasoning: usage.Reasoning, Total: usage.Total,
			}
		}
		encoder := json.NewEncoder(stdout)
		encoder.SetEscapeHTML(false)
		return encoder.Encode(result)
	}
	output := newTurnOutput(stdout, isTerminal(stdout))
	turnResult, err := run(ctx, output.Sink)
	if err != nil {
		return err
	}
	if err := output.Finish(turnResult.Final); err != nil {
		return err
	}
	printTurnUsage(stdout, turnResult.Final, isTerminal(stdout))
	return nil
}

type headlessJSONResult struct {
	Schema  string            `json:"schema"`
	Content string            `json:"content"`
	Usage   headlessJSONUsage `json:"usage"`
}

type headlessJSONUsage struct {
	Input     int `json:"input_tokens,omitempty"`
	Cached    int `json:"cached_tokens,omitempty"`
	Output    int `json:"output_tokens,omitempty"`
	Reasoning int `json:"reasoning_tokens,omitempty"`
	Total     int `json:"total_tokens,omitempty"`
}

func printTurnUsage(writer io.Writer, message *schema.Message, color bool) {
	if message == nil || message.ResponseMeta == nil || message.ResponseMeta.Usage == nil {
		return
	}
	usage := tokenusage.FromMessage(message)
	usageLine := fmt.Sprintf("usage: input=%d", usage.Input)
	if usage.Cached > 0 {
		usageLine += fmt.Sprintf(" cached=%d cache_hit=%.1f%%", usage.Cached, usage.CacheHitPercent())
	}
	usageLine += fmt.Sprintf(" output=%d reasoning=%d total=%d\n", usage.Output, usage.Reasoning, usage.Total)
	_ = render.WriteStyled(writer, usageLine, render.MetadataColor, color)
}

type scanResult struct {
	line string
	err  error
	done bool
}

func scanInput(ctx context.Context, scanner *bufio.Scanner) <-chan scanResult {
	results := make(chan scanResult)
	go func() {
		defer close(results)
		for scanner.Scan() {
			select {
			case results <- scanResult{line: scanner.Text()}:
			case <-ctx.Done():
				return
			}
		}
		result := scanResult{err: scanner.Err(), done: scanner.Err() == nil}
		select {
		case results <- result:
		case <-ctx.Done():
		}
	}()
	return results
}
