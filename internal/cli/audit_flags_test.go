package cli

import "testing"

func TestProfileArgumentAcceptsEveryFlagSetSpelling(t *testing.T) {
	for _, name := range []string{"--profile", "-profile", "--p", "-p"} {
		for _, arguments := range [][]string{{name, "chosen"}, {name + "=chosen"}} {
			value, set, err := profileArgument(arguments)
			if err != nil || !set || value != "chosen" {
				t.Fatalf("%v: %q %v %v", arguments, value, set, err)
			}
		}
	}
}

func TestEvaluationProtectedFlagsIncludeSingleHyphen(t *testing.T) {
	for _, name := range []string{"--workspace", "-workspace", "--workspace=x", "-workspace=x"} {
		if !hasCLIFlag([]string{name}, "workspace") {
			t.Fatalf("protected override missed: %s", name)
		}
	}
}
