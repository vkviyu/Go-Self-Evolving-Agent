package agentgoal

import "testing"

func TestAutoActivationRequiresExplicitAdmission(t *testing.T) {
	config := Config{Enabled: true}.WithDefaults()
	if config.RequiresInitialGoal() {
		t.Fatal("auto mode prestarted a goal without admission")
	}
}

func TestActivationOverrides(t *testing.T) {
	if !(Config{Enabled: true, Activation: ActivationAlways}).RequiresInitialGoal() {
		t.Fatal("always mode bypassed a greeting")
	}
	if (Config{Enabled: false, Activation: ActivationAlways}).RequiresInitialGoal() {
		t.Fatal("disabled mode activated a goal")
	}
	if err := (Config{Enabled: true, Activation: "invalid"}).Validate(); err == nil {
		t.Fatal("invalid activation accepted")
	}
}
