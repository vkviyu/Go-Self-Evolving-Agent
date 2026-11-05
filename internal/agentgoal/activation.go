package agentgoal

// Activation controls admission to the Outer Loop, independently of how an
// admitted goal is assessed. Auto admits explicit contracts through goal_start
// inside the existing Eino turn; it does not classify text using keywords.
type Activation string

const (
	ActivationAuto   Activation = "auto"
	ActivationAlways Activation = "always"
)

// RequiresInitialGoal reports whether the host starts a goal before execution.
// Auto initially runs an ordinary turn with optional task admission.
func (config Config) RequiresInitialGoal() bool {
	return config.Enabled && config.Activation == ActivationAlways
}
