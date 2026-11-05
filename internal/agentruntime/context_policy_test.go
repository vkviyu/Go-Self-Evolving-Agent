package agentruntime

import "testing"

func TestContextPlanThresholds(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tokens int
		force  bool
		want   contextPlan
	}{
		{"below half", 499, false, contextPlan{contextLevelNormal, false, 250}},
		{"at half", 500, false, contextPlan{contextLevelNormal, false, 250}},
		{"above half", 501, false, contextPlan{contextLevelNormal, false, 250}},
		{"below trigger", 749, false, contextPlan{contextLevelNormal, false, 250}},
		{"at trigger", 750, false, contextPlan{contextLevelElevated, true, 250}},
		{"above trigger", 751, false, contextPlan{contextLevelElevated, true, 250}},
		{"below critical", 899, false, contextPlan{contextLevelElevated, true, 250}},
		{"at critical", 900, false, contextPlan{contextLevelCritical, true, 166}},
		{"over window", 1200, false, contextPlan{contextLevelCritical, true, 166}},
		{"force normal", 100, true, contextPlan{contextLevelNormal, true, 333}},
		{"force elevated", 750, true, contextPlan{contextLevelElevated, true, 250}},
		{"force critical", 900, true, contextPlan{contextLevelCritical, true, 166}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := planContext(1000, 750, tc.tokens, tc.force); got != tc.want {
				t.Fatalf("plan=%+v want=%+v", got, tc.want)
			}
		})
	}
	// A custom trigger above 90% must not postpone critical pressure, but the
	// compaction trigger remains a separate decision (the historical policy).
	if got := planContext(1000, 950, 900, false); got != (contextPlan{contextLevelCritical, false, 166}) {
		t.Fatalf("custom trigger changed critical precedence: %+v", got)
	}
}
