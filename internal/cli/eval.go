package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/app"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/evaluation"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

var headlessUsagePattern = regexp.MustCompile(`usage: input=(\d+)(?: cached=(\d+) cache_hit=[0-9.]+%)? output=(\d+) reasoning=(\d+) total=(\d+)`)

func runEval(ctx context.Context, arguments []string, stdout, stderr io.Writer, deps dependencies) error {
	if len(arguments) == 0 {
		printEvalUsage(stderr)
		return errors.New("eval requires a subcommand")
	}
	switch arguments[0] {
	case "doctor":
		return runEvalDoctor(ctx, arguments[1:], stdout, stderr)
	case "run":
		return runEvalTasks(ctx, arguments[1:], stdout, stderr, deps)
	case "validate":
		return runEvalValidate(arguments[1:], stdout, stderr)
	case "export-swebench":
		return runEvalExportSWE(arguments[1:], stdout, stderr)
	case "summarize":
		return runEvalSummarize(arguments[1:], stdout, stderr)
	case "score":
		return runEvalScore(arguments[1:], stdout, stderr)
	case "help", "-h", "--help":
		printEvalUsage(stdout)
		return nil
	default:
		return fmt.Errorf("unknown eval subcommand %q", arguments[0])
	}
}

func printEvalUsage(writer io.Writer) {
	fmt.Fprintln(writer, "usage: gsea eval <command>")
	fmt.Fprintln(writer, "commands:")
	fmt.Fprintln(writer, "  doctor           inspect local benchmark runtimes and dependencies")
	fmt.Fprintln(writer, "  run              run GSEA against a JSONL task manifest")
	fmt.Fprintln(writer, "  validate         validate a JSONL task manifest")
	fmt.Fprintln(writer, "  export-swebench  convert GSEA result JSONL to SWE-bench predictions")
	fmt.Fprintln(writer, "  summarize        aggregate one or more result JSONL files")
	fmt.Fprintln(writer, "  score            score official Harbor or SWE-bench results")
}

func runEvalDoctor(ctx context.Context, arguments []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("eval doctor", flag.ContinueOnError)
	flags.SetOutput(stderr)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("eval doctor does not accept positional arguments")
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(evaluation.InspectCapabilities(ctx))
}

func runEvalTasks(ctx context.Context, arguments []string, stdout, stderr io.Writer, deps dependencies) error {
	flags := flag.NewFlagSet("eval run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	tasksPath := flags.String("tasks", "", "path to gsea.eval.task.v1 JSONL")
	outputPath := flags.String("output", "", "result JSONL path; defaults below the evaluation root")
	root := flags.String("root", "", "evaluation run directory; omitted creates an OS temporary directory")
	runID := flags.String("run-id", "", "stable run identifier; omitted generates one")
	modelLabel := flags.String("model-label", "", "model label written to results; defaults to chat configuration")
	taskFilter := flags.String("task", "", "run only one task ID")
	keepWorktrees := flags.Bool("keep-worktrees", false, "retain task worktrees after verification")
	failOnUnresolved := flags.Bool("fail-on-unresolved", false, "return an error when any required verifier fails")
	parallel := flags.Int("parallel", 1, "maximum number of evaluation tasks to run concurrently")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if strings.TrimSpace(*tasksPath) == "" {
		return errors.New("eval run requires --tasks")
	}
	if *parallel <= 0 || *parallel > 64 {
		return errors.New("eval run parallelism must be between 1 and 64")
	}
	chatArguments := flags.Args()
	for _, protected := range []string{"evaluation-mode", "workspace", "data-dir", "session"} {
		if hasCLIFlag(chatArguments, protected) {
			return fmt.Errorf("chat argument --%s is owned by the evaluator", protected)
		}
	}
	manifest, err := os.Open(*tasksPath)
	if err != nil {
		return fmt.Errorf("open evaluation tasks: %w", err)
	}
	tasks, loadErr := evaluation.LoadTasks(manifest)
	closeErr := manifest.Close()
	if loadErr != nil || closeErr != nil {
		return errors.Join(loadErr, closeErr)
	}
	if *taskFilter != "" {
		filtered := tasks[:0]
		for _, task := range tasks {
			if task.ID == *taskFilter {
				filtered = append(filtered, task)
			}
		}
		tasks = filtered
		if len(tasks) == 0 {
			return fmt.Errorf("task %q was not found", *taskFilter)
		}
	}
	evaluationRoot := strings.TrimSpace(*root)
	if evaluationRoot == "" {
		evaluationRoot, err = os.MkdirTemp("", "gsea-eval-")
	} else {
		evaluationRoot, err = filepath.Abs(evaluationRoot)
		if err == nil {
			err = os.MkdirAll(evaluationRoot, 0o700)
		}
	}
	if err != nil {
		return fmt.Errorf("prepare evaluation root: %w", err)
	}
	if strings.TrimSpace(*runID) == "" {
		generated, generateErr := newSessionID()
		if generateErr != nil {
			return generateErr
		}
		*runID = "eval-" + strings.TrimPrefix(string(generated), "chat-")
	}
	if strings.TrimSpace(*modelLabel) == "" {
		*modelLabel = resolveEvalModelLabel(chatArguments)
	}
	resultPath := strings.TrimSpace(*outputPath)
	if resultPath == "" {
		resultPath = filepath.Join(evaluationRoot, "results-"+safeCLIComponent(*runID)+".jsonl")
	} else if resultPath, err = filepath.Abs(resultPath); err != nil {
		return fmt.Errorf("resolve result path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(resultPath), 0o700); err != nil {
		return fmt.Errorf("create result directory: %w", err)
	}
	resultFile, err := os.OpenFile(resultPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create result JSONL: %w", err)
	}

	agent := evalAgentFunc(deps, chatArguments, *runID)
	runner, err := evaluation.NewRunner(evaluation.RunnerConfig{
		Root: evaluationRoot, RunID: *runID, Model: *modelLabel,
		KeepWorktrees: *keepWorktrees, Agent: agent, Verifier: deps.evalVerifier,
	})
	if err != nil {
		_ = resultFile.Close()
		return err
	}
	completed := 0
	results, runErr := executeEvaluationTasks(ctx, tasks, *parallel, runner.RunTask, func(result evaluation.Result) error {
		if err := evaluation.WriteResult(resultFile, result); err != nil {
			return err
		}
		completed++
		fmt.Fprintf(stderr, "eval: [%d/%d] %s resolved=%t duration_ms=%d\n", completed, len(tasks), result.TaskID, result.Resolved, result.DurationMS)
		return nil
	})
	resultCloseErr := resultFile.Close()
	if runErr != nil || resultCloseErr != nil {
		return errors.Join(runErr, resultCloseErr)
	}
	summary := evaluation.Summarize(results)
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(map[string]any{"schema": "gsea.eval.summary.v1", "results_path": resultPath, "evaluation_root": evaluationRoot, "summary": summary}); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if *failOnUnresolved && summary.Failed > 0 {
		return fmt.Errorf("%d of %d evaluation tasks were unresolved", summary.Failed, summary.Tasks)
	}
	return nil
}

type evaluationTaskFunc func(context.Context, evaluation.Task) evaluation.Result

func executeEvaluationTasks(
	ctx context.Context,
	tasks []evaluation.Task,
	parallel int,
	run evaluationTaskFunc,
	consume func(evaluation.Result) error,
) ([]evaluation.Result, error) {
	if parallel <= 0 || run == nil || consume == nil {
		return nil, errors.New("evaluation executor requires positive parallelism, runner and result consumer")
	}
	if len(tasks) == 0 {
		return nil, nil
	}
	if parallel > len(tasks) {
		parallel = len(tasks)
	}
	runContext, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan evaluation.Task)
	outcomes := make(chan evaluation.Result)
	var workers sync.WaitGroup
	workers.Add(parallel)
	for range parallel {
		go func() {
			defer workers.Done()
			for task := range jobs {
				result := run(runContext, task)
				outcomes <- result
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, task := range tasks {
			select {
			case jobs <- task:
			case <-runContext.Done():
				return
			}
		}
	}()
	go func() {
		workers.Wait()
		close(outcomes)
	}()
	results := make([]evaluation.Result, 0, len(tasks))
	var consumeErr error
	for result := range outcomes {
		results = append(results, result)
		if consumeErr == nil {
			if err := consume(result); err != nil {
				consumeErr = fmt.Errorf("write task result: %w", err)
				cancel()
			}
		}
	}
	if consumeErr != nil {
		return results, consumeErr
	}
	if err := ctx.Err(); err != nil {
		return results, err
	}
	return results, nil
}

func evalAgentFunc(deps dependencies, baseArguments []string, runID string) evaluation.AgentFunc {
	return func(ctx context.Context, task evaluation.AgentRequest, workspace, runDirectory string) (evaluation.AgentOutcome, error) {
		dataDirectory := filepath.Join(runDirectory, "data")
		sessionID := evaluationSessionID(runID, task.TaskID)
		arguments := append([]string(nil), baseArguments...)
		arguments = append(arguments,
			"--evaluation-mode",
			"--workspace", workspace,
			"--data-dir", dataDirectory,
			"--session", sessionID,
		)
		var transcript, diagnostics bytes.Buffer
		// Capture the selected profile and authority once. Usage must reopen
		// the same native store even if configuration changes during the turn.
		resolved, err := loadChatConfig(arguments)
		var options chatOptions
		if err == nil {
			options, err = parseChatOptions(arguments, resolved, &diagnostics)
		}
		configErr := err
		if configErr == nil {
			resolved.DataDirectory = options.dataDirectory
			resolved.WorkspaceDirectory = options.workspaceDirectory
			err = runConfiguredChat(ctx, resolved, options, bytes.NewReader(nil), &transcript, &diagnostics, deps, &task.ProblemStatement)
		}
		transcriptPath := filepath.Join(runDirectory, "transcript.txt")
		stderrPath := filepath.Join(runDirectory, "agent.stderr.txt")
		writeErr := errors.Join(
			os.WriteFile(transcriptPath, transcript.Bytes(), 0o600),
			os.WriteFile(stderrPath, diagnostics.Bytes(), 0o600),
		)
		outcome := evaluation.AgentOutcome{
			Status: "completed", SessionID: sessionID,
			TranscriptPath: transcriptPath, StderrPath: stderrPath,
			Usage: parseHeadlessUsage(transcript.String()),
		}
		usageContext, cancelUsage := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		var usage evaluation.Usage
		usageErr := configErr
		if usageErr == nil {
			usage, usageErr = readEvaluationUsage(usageContext, resolved, sessionID)
		}
		cancelUsage()
		if usageErr == nil {
			outcome.Usage = usage
		} else {
			outcome.Error = "read durable usage: " + usageErr.Error()
		}
		if err != nil {
			outcome.Status = "failed"
		}
		if writeErr != nil {
			return outcome, errors.Join(err, fmt.Errorf("write Agent logs: %w", writeErr))
		}
		return outcome, err
	}
}

func readEvaluationUsage(ctx context.Context, cfg config.Config, sessionID string) (evaluation.Usage, error) {
	report, err := app.ReadSessionUsage(ctx, cfg, session.ID(sessionID))
	if err != nil {
		return evaluation.Usage{}, err
	}
	aggregate := report.Aggregate
	return evaluation.Usage{
		Input: aggregate.InputTokens, Cached: aggregate.CachedTokens,
		Output: aggregate.OutputTokens, Reasoning: aggregate.ReasoningTokens,
		Total: aggregate.TotalTokens, Records: aggregate.Records, CostMicros: aggregate.CostMicros,
	}, nil
}

func parseHeadlessUsage(transcript string) evaluation.Usage {
	matches := headlessUsagePattern.FindAllStringSubmatch(transcript, -1)
	var total evaluation.Usage
	for _, match := range matches {
		if len(match) != 6 {
			continue
		}
		input, _ := strconv.Atoi(match[1])
		cached, _ := strconv.Atoi(match[2])
		output, _ := strconv.Atoi(match[3])
		reasoning, _ := strconv.Atoi(match[4])
		tokens, _ := strconv.Atoi(match[5])
		total.Input += input
		total.Cached += cached
		total.Output += output
		total.Reasoning += reasoning
		total.Total += tokens
	}
	return total
}

func evaluationSessionID(runID, taskID string) string {
	raw := runID + "\x00" + taskID
	digest := sha256.Sum256([]byte(raw))
	base := safeCLIComponent(runID + "-" + taskID)
	if len(base) > 108 {
		base = base[:108]
	}
	return fmt.Sprintf("eval-%s-%x", base, digest[:4])
}

func safeCLIComponent(value string) string {
	var safe strings.Builder
	for _, item := range value {
		if item >= 'a' && item <= 'z' || item >= 'A' && item <= 'Z' || item >= '0' && item <= '9' || item == '-' || item == '_' || item == '.' {
			safe.WriteRune(item)
		} else {
			safe.WriteByte('_')
		}
	}
	result := strings.Trim(safe.String(), ".")
	if result == "" {
		result = "task"
	}
	if len(result) > 120 {
		result = result[:120]
	}
	return result
}

func hasCLIFlag(arguments []string, name string) bool {
	for _, argument := range arguments {
		if !strings.HasPrefix(argument, "-") {
			continue
		}
		option := strings.TrimLeft(argument, "-")
		if option == name || strings.HasPrefix(option, name+"=") {
			return true
		}
	}
	return false
}

func resolveEvalModelLabel(chatArguments []string) string {
	for index := range chatArguments {
		argument := chatArguments[index]
		if argument == "--model" && index+1 < len(chatArguments) {
			return chatArguments[index+1]
		}
		if model, ok := strings.CutPrefix(argument, "--model="); ok {
			return model
		}
	}
	profile, set, err := profileArgument(chatArguments)
	loadOptions := config.LoadOptions{}
	if err == nil && set {
		loadOptions.Profile = profile
	}
	if resolved, loadErr := config.Load(loadOptions); loadErr == nil && strings.TrimSpace(resolved.Model.Name) != "" {
		return resolved.Model.Name
	}
	return "configured-model"
}

func runEvalValidate(arguments []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("eval validate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	tasksPath := flags.String("tasks", "", "path to gsea.eval.task.v1 JSONL")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if strings.TrimSpace(*tasksPath) == "" || flags.NArg() != 0 {
		return errors.New("eval validate requires --tasks and no positional arguments")
	}
	file, err := os.Open(*tasksPath)
	if err != nil {
		return err
	}
	tasks, loadErr := evaluation.LoadTasks(file)
	closeErr := file.Close()
	if loadErr != nil || closeErr != nil {
		return errors.Join(loadErr, closeErr)
	}
	return json.NewEncoder(stdout).Encode(map[string]any{"schema": "gsea.eval.validation.v1", "valid": true, "tasks": len(tasks)})
}

func runEvalExportSWE(arguments []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("eval export-swebench", flag.ContinueOnError)
	flags.SetOutput(stderr)
	resultsPath := flags.String("results", "", "path to gsea.eval.result.v1 JSONL")
	outputPath := flags.String("output", "-", "prediction JSONL path or - for stdout")
	model := flags.String("model", "", "override model_name_or_path")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if strings.TrimSpace(*resultsPath) == "" || flags.NArg() != 0 {
		return errors.New("eval export-swebench requires --results and no positional arguments")
	}
	input, err := os.Open(*resultsPath)
	if err != nil {
		return err
	}
	results, loadErr := evaluation.LoadResults(input)
	closeErr := input.Close()
	if loadErr != nil || closeErr != nil {
		return errors.Join(loadErr, closeErr)
	}
	writer := stdout
	var file *os.File
	if *outputPath != "-" {
		file, err = os.OpenFile(*outputPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		writer = file
	}
	writeErr := evaluation.WriteSWEPredictions(writer, results, *model)
	if file != nil {
		writeErr = errors.Join(writeErr, file.Close())
	}
	return writeErr
}

type repeatedStrings []string

func (values *repeatedStrings) String() string { return strings.Join(*values, ",") }

func (values *repeatedStrings) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("result path must not be empty")
	}
	*values = append(*values, value)
	return nil
}

func runEvalSummarize(arguments []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("eval summarize", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var paths repeatedStrings
	flags.Var(&paths, "results", "result JSONL path; repeat for multiple attempts")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if len(paths) == 0 || flags.NArg() != 0 {
		return errors.New("eval summarize requires one or more --results and no positional arguments")
	}
	all := make([]evaluation.Result, 0)
	for _, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		results, loadErr := evaluation.LoadResults(file)
		closeErr := file.Close()
		if loadErr != nil || closeErr != nil {
			return errors.Join(loadErr, closeErr)
		}
		all = append(all, results...)
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(map[string]any{"schema": "gsea.eval.summary.v1", "summary": evaluation.Summarize(all)})
}

func runEvalScore(arguments []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("eval score", flag.ContinueOnError)
	flags.SetOutput(stderr)
	benchmark := flags.String("benchmark", "", "official benchmark protocol: terminal-bench-2.1 or swe-bench-verified")
	var inputs repeatedStrings
	flags.Var(&inputs, "input", "Harbor job directory/result JSON (repeatable) or one SWE-bench results.json")
	harnessVersion := flags.String("harness-version", "", "assert the harness version; fills it only when the result does not record one")
	model := flags.String("model", "", "assert the model identifier; fills it only when the result does not record one")
	reasoningEffort := flags.String("reasoning-effort", "", "assert the reasoning effort; fills it only when the result does not record one")
	disqualifications := flags.String("disqualifications", "", "official Terminal-Bench reward-hacking audit JSON")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if strings.TrimSpace(*benchmark) == "" || len(inputs) == 0 || flags.NArg() != 0 {
		return errors.New("eval score requires --benchmark and --input with no positional arguments")
	}
	var score evaluation.Scorecard
	switch *benchmark {
	case evaluation.BenchmarkTerminalBench21:
		trials, metadata, err := evaluation.LoadHarborJobs(inputs)
		if err != nil {
			return err
		}
		disqualified := 0
		if strings.TrimSpace(*disqualifications) != "" {
			disqualified, err = evaluation.ApplyTerminalBenchAudit(trials, strings.TrimSpace(*disqualifications))
			if err != nil {
				return err
			}
		}
		score, err = evaluation.ScoreTerminalBench(trials)
		if err != nil {
			return err
		}
		score.Agent = metadata.Agent
		score.AgentVersion = metadata.AgentVersion
		score.Model = metadata.Model
		score.ReasoningEffort = metadata.ReasoningEffort
		evaluation.ApplyHarborProtocolMetadata(&score, metadata)
		if metadata.Source != "" {
			score.Metadata = map[string]any{
				"harbor_source":        metadata.Source,
				"dataset_ref":          metadata.DatasetRef,
				"exception_counts":     metadata.ExceptionCounts,
				"reward_audit_applied": strings.TrimSpace(*disqualifications) != "",
				"disqualified_trials":  disqualified,
			}
		}
	case evaluation.BenchmarkSWEBenchVerified:
		if len(inputs) != 1 {
			return errors.New("swe-bench-verified accepts exactly one --input results.json")
		}
		if strings.TrimSpace(*disqualifications) != "" {
			return errors.New("--disqualifications is only valid for terminal-bench-2.1")
		}
		report, err := evaluation.LoadSWEBenchRunReport(inputs[0])
		if err != nil {
			return err
		}
		score = evaluation.ScoreSWEBenchVerified(report)
	default:
		return fmt.Errorf("unsupported benchmark protocol %q", *benchmark)
	}
	if err := assertOrFillScoreLabel(&score.HarnessVersion, *harnessVersion, "harness version"); err != nil {
		return err
	}
	if err := assertOrFillScoreLabel(&score.Model, *model, "model"); err != nil {
		return err
	}
	if err := assertOrFillScoreLabel(&score.ReasoningEffort, *reasoningEffort, "reasoning effort"); err != nil {
		return err
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	return encoder.Encode(score)
}

func assertOrFillScoreLabel(recorded *string, requested, label string) error {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return nil
	}
	if *recorded == "" {
		*recorded = requested
		return nil
	}
	if *recorded != requested {
		return fmt.Errorf("%s mismatch: result records %q, flag asserts %q", label, *recorded, requested)
	}
	return nil
}
