package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/cloudwego/eino/components/model"

	"github.com/vkviyu/go-self-evolving-agent/internal/evaluation"
	"github.com/vkviyu/go-self-evolving-agent/internal/modelgateway"
)

type dependencies struct {
	newModel     func(context.Context, modelgateway.Config) (model.ToolCallingChatModel, error)
	evalVerifier evaluation.VerifierFunc
}

var productionDependencies = dependencies{newModel: modelgateway.New}

// Run executes the gsea command tree and returns its process exit code.
// The executable package owns signal handling; this package owns command
// parsing and application orchestration.
func Run(ctx context.Context, arguments []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return runWithDependencies(ctx, arguments, stdin, stdout, stderr, productionDependencies)
}

func run(ctx context.Context, arguments []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return runWithDependencies(ctx, arguments, stdin, stdout, stderr, productionDependencies)
}

func runWithDependencies(ctx context.Context, arguments []string, stdin io.Reader, stdout, stderr io.Writer, deps dependencies) int {
	if len(arguments) == 0 {
		printUsage(stderr)
		return 2
	}
	switch arguments[0] {
	case "strategy":
		if err := runStrategy(ctx, arguments[1:], stdout, stderr, deps); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 0
			}
			fmt.Fprintf(stderr, "gsea strategy: %v\n", err)
			return 1
		}
		return 0
	case "maintenance":
		if err := runMaintenance(ctx, arguments[1:], stdout, stderr); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 0
			}
			fmt.Fprintf(stderr, "gsea maintenance: %v\n", err)
			return 1
		}
		return 0
	case "chat":
		if err := runChat(ctx, arguments[1:], stdin, stdout, stderr, deps); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 0
			}
			fmt.Fprintf(stderr, "gsea chat: %v\n", err)
			if errors.Is(err, context.Canceled) {
				return 130
			}
			return 1
		}
		return 0
	case "resume":
		if err := runResume(ctx, arguments[1:], stdin, stdout, stderr, deps); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 0
			}
			fmt.Fprintf(stderr, "gsea resume: %v\n", err)
			if errors.Is(err, context.Canceled) {
				return 130
			}
			return 1
		}
		return 0
	case "doctor":
		if err := runDoctor(ctx, arguments[1:], stdout, stderr); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 0
			}
			fmt.Fprintf(stderr, "gsea doctor: %v\n", err)
			return 1
		}
		return 0
	case "control":
		if err := runControl(ctx, arguments[1:], stdin, stdout, stderr); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 0
			}
			fmt.Fprintf(stderr, "gsea control: %v\n", err)
			if errors.Is(err, context.Canceled) {
				return 130
			}
			return 1
		}
		return 0
	case "eval":
		if err := runEval(ctx, arguments[1:], stdout, stderr, deps); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 0
			}
			fmt.Fprintf(stderr, "gsea eval: %v\n", err)
			if errors.Is(err, context.Canceled) {
				return 130
			}
			return 1
		}
		return 0
	case "session":
		if err := runSession(ctx, arguments[1:], stdout, stderr); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 0
			}
			fmt.Fprintf(stderr, "gsea session: %v\n", err)
			return 1
		}
		return 0
	case "help", "-h", "--help":
		printUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", arguments[0])
		printUsage(stderr)
		return 2
	}
}

func printUsage(writer io.Writer) {
	fmt.Fprintln(writer, "usage: gsea <command>")
	fmt.Fprintln(writer, "commands:")
	fmt.Fprintln(writer, "  chat     start or resume a local Agent conversation")
	fmt.Fprintln(writer, "  resume   browse or reopen saved conversations in the current workspace")
	fmt.Fprintln(writer, "  doctor   verify the local runtime without contacting a model")
	fmt.Fprintln(writer, "  control  query or control durable Agent activity using JSONL")
	fmt.Fprintln(writer, "  eval     run and export language-neutral coding Agent evaluations")
	fmt.Fprintln(writer, "  session  create, list, inspect and remove managed Agent Sessions")
	fmt.Fprintln(writer, "  maintenance  preview/close finished recovery and compact freed database pages")
	fmt.Fprintln(writer, "  strategy  propose, verify, publish and roll back bounded LoopSpecs")
}
