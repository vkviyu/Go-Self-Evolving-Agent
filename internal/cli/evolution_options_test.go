package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/config"
)

func parseEvolutionOptions(command string, args []string, output io.Writer) (config.Config, string, error) {
	switch command {
	case "maintenance":
		options, err := parseMaintenanceOptions(args, output)
		return options.config, options.action, err
	case "plugins":
		options, err := parsePluginOptions(args, output)
		return options.config, options.action, err
	case "strategy":
		options, err := parseStrategyOptions(args, output)
		return options.config, options.action, err
	default:
		panic("unknown test command")
	}
}

func withDataDirectory(args []string, data string) []string {
	if len(args) == 0 {
		return nil
	}
	return append([]string{args[0], "--data-dir", data}, args[1:]...)
}

func TestEvolutionOptionsAcceptEveryOperationWithoutOpeningRuntime(t *testing.T) {
	t.Setenv(config.HomeEnvironment, t.TempDir())
	for _, tc := range []struct {
		command string
		args    []string
	}{
		{"maintenance", []string{"plan", "--session", "owner"}},
		{"maintenance", []string{"plan", "--session", "owner", "--confirm=false"}},
		{"maintenance", []string{"archive", "--session", "owner", "--plan-id", "plan", "--confirm"}},
		{"maintenance", []string{"compact", "--confirm"}},
		{"plugins", []string{"list"}},
		{"plugins", []string{"build", "--request", "not-read-yet.json"}},
		{"plugins", []string{"inspect-build", "--build-id", "build"}},
		{"plugins", []string{"stage", "--manifest", "not-read-yet.json", "--wasm", "not-read-yet.wasm"}},
		{"plugins", []string{"test", "--tool", "extension_test", "--revision", "v1", "--cases", "not-read-yet.json"}},
		{"plugins", []string{"activate", "--tool", "extension_test", "--revision", "v1", "--report-id", "report", "--confirm"}},
		{"plugins", []string{"rollback", "--tool", "extension_test", "--revision", "v1", "--report-id", "report", "--confirm"}},
		{"plugins", []string{"retire", "--tool", "extension_test", "--revision", "v1", "--confirm"}},
		{"plugins", []string{"gc-plan", "--tool", "extension_test", "--revision", "v1"}},
		{"plugins", []string{"collect", "--tool", "extension_test", "--revision", "v1", "--plan-id", "plan", "--confirm"}},
		{"strategy", []string{"propose", "--file", "not-read-yet.json"}},
		{"strategy", []string{"suite", "--file", "not-read-yet.json", "--confirm"}},
		{"strategy", []string{"inspect", "--candidate-id", "candidate"}},
		{"strategy", []string{"report", "--report-id", "report"}},
		{"strategy", []string{"shadow", "--candidate-id", "candidate", "--suite-id", "suite", "--confirm", "--", "--model", "test", "--session", "owner"}},
		{"strategy", []string{"publish", "--report-id", "report", "--confirm"}},
		{"strategy", []string{"rollback", "--baseline-id", "baseline", "--candidate-id", "candidate", "--confirm"}},
	} {
		t.Run(tc.command+"/"+strings.Join(tc.args, " "), func(t *testing.T) {
			data := filepath.Join(t.TempDir(), "unopened")
			cfg, action, err := parseEvolutionOptions(tc.command, withDataDirectory(tc.args, data), io.Discard)
			if err != nil || action != tc.args[0] || cfg.DataDirectory != data {
				t.Fatalf("action=%s data=%s err=%v", action, cfg.DataDirectory, err)
			}
			if _, err := os.Stat(data); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("parser acquired runtime resources: %v", err)
			}
		})
	}
}

func TestEvolutionOptionsPreserveValidationErrorsAndStopBeforeExecution(t *testing.T) {
	t.Setenv(config.HomeEnvironment, t.TempDir())
	for _, tc := range []struct {
		command string
		args    []string
		want    string
	}{
		{"maintenance", nil, "maintenance requires plan, archive or compact"},
		{"maintenance", []string{"unknown"}, "unknown maintenance operation"},
		{"maintenance", []string{"plan", "extra"}, "maintenance accepts no positional arguments"},
		{"maintenance", []string{"plan", "--session", "../escape"}, "plan requires --session only; it does not apply changes"},
		{"maintenance", []string{"plan", "--session", "owner", "--confirm"}, "plan requires --session only; it does not apply changes"},
		{"maintenance", []string{"archive", "--session", "owner", "--plan-id", "plan"}, "archive requires --session, --plan-id and --confirm"},
		{"maintenance", []string{"compact", "--confirm", "--session", "owner"}, "compact requires --confirm and does not accept session/plan selection"},
		{"plugins", nil, "plugins requires list, stage, build, inspect-build, test, activate, retire, rollback, gc-plan or collect"},
		{"plugins", []string{"unknown"}, "unknown plugins operation"},
		{"plugins", []string{"list", "extra"}, "plugins accepts no positional arguments"},
		{"plugins", []string{"list", "--confirm=false"}, "--confirm is not valid for plugins list"},
		{"plugins", []string{"stage", "--tool", "extension_test"}, "--tool is not valid for plugins stage"},
		{"plugins", []string{"test"}, "operation requires exact --tool and --revision"},
		{"plugins", []string{"activate", "--tool", "extension_test", "--revision", "v1", "--confirm"}, "publication requires --report-id and --confirm"},
		{"plugins", []string{"retire", "--tool", "extension_test", "--revision", "v1"}, "operation requires --confirm"},
		{"plugins", []string{"collect", "--tool", "extension_test", "--revision", "v1", "--confirm"}, "collection requires --plan-id"},
		{"plugins", []string{"build"}, "build requires --request"},
		{"plugins", []string{"inspect-build"}, "inspect-build requires --build-id"},
		{"plugins", []string{"stage"}, "stage requires --manifest and --wasm"},
		{"plugins", []string{"test", "--tool", "extension_test", "--revision", "v1"}, "test requires --cases"},
		{"strategy", nil, "strategy requires propose, suite, inspect, report, shadow, publish or rollback"},
		{"strategy", []string{"unknown"}, "unknown strategy operation"},
		{"strategy", []string{"inspect", "extra"}, "flags do not match strategy operation"},
		{"strategy", []string{"inspect", "--candidate-id", "candidate", "--confirm=false"}, "flags do not match strategy operation"},
		{"strategy", []string{"suite", "--file", "missing.json"}, "operation requires --confirm"},
		{"strategy", []string{"propose"}, "operation requires --file"},
		{"strategy", []string{"shadow", "--confirm"}, "operation requires --candidate-id"},
		{"strategy", []string{"publish", "--confirm"}, "operation requires --report-id"},
		{"strategy", []string{"rollback", "--candidate-id", "candidate", "--confirm"}, "rollback requires --baseline-id"},
		{"strategy", []string{"shadow", "--candidate-id", "candidate", "--confirm"}, "shadow requires --suite-id"},
		{"strategy", []string{"shadow", "--candidate-id", "candidate", "--suite-id", "suite", "--confirm", "--", "--profile", "x"}, "put --profile before the -- separator so its defaults are loaded"},
		{"strategy", []string{"shadow", "--candidate-id", "candidate", "--suite-id", "suite", "--confirm", "--", "--session", "owner", "--list-recoveries"}, "shadow accepts model/config flags, not chat/recovery/evaluation operations"},
	} {
		t.Run(tc.command+"/"+strings.Join(tc.args, " "), func(t *testing.T) {
			data := filepath.Join(t.TempDir(), "unopened")
			args := withDataDirectory(tc.args, data)
			_, _, err := parseEvolutionOptions(tc.command, args, io.Discard)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("error=%v want=%s", err, tc.want)
			}
			var out, log bytes.Buffer
			code := runWithDependencies(context.Background(), append([]string{tc.command}, args...), unreadAbandonmentInput{t}, &out, &log, dependencies{})
			if code != 1 || out.Len() != 0 || !strings.Contains(log.String(), tc.want) {
				t.Fatalf("exit=%d stdout=%s stderr=%s", code, out.String(), log.String())
			}
			if _, err := os.Stat(data); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid command opened runtime: %v", err)
			}
		})
	}
}

func TestEvolutionHelpDoesNotOpenRuntime(t *testing.T) {
	t.Setenv(config.HomeEnvironment, t.TempDir())
	for _, tc := range []struct{ command, action string }{{"maintenance", "plan"}, {"plugins", "stage"}, {"strategy", "shadow"}} {
		data := filepath.Join(t.TempDir(), "unopened")
		args := []string{tc.action, "--data-dir", data, "--help"}
		var help bytes.Buffer
		if _, _, err := parseEvolutionOptions(tc.command, args, &help); !errors.Is(err, flag.ErrHelp) || help.Len() == 0 {
			t.Fatalf("help=%s err=%v", help.String(), err)
		}
		var out, log bytes.Buffer
		if code := runWithDependencies(context.Background(), append([]string{tc.command}, args...), unreadAbandonmentInput{t}, &out, &log, dependencies{}); code != 0 || out.Len() != 0 || log.Len() == 0 {
			t.Fatalf("help exit=%d stdout=%s stderr=%s", code, out.String(), log.String())
		}
		if _, err := os.Stat(data); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("help opened runtime: %v", err)
		}
	}
}
