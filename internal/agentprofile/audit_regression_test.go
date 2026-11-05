package agentprofile

import (
	"testing"
	"time"
)

func TestDelegationNarrowsAllAncestorLimits(t *testing.T) {
	parent, child := validProfile("parent"), validProfile("child")
	parent.Limits = Limits{MaxIterations: 2, MaxToolCalls: 3, MaxChildren: 0, MaxInheritedTokens: 11, MaxInputTokens: 13, MaxOutputTokens: 17, MaxWallTime: time.Second}
	child.Limits = Limits{MaxIterations: 20, MaxToolCalls: 30, MaxChildren: 4, MaxWallTime: time.Minute}
	got, err := ConstrainDelegation(parent, child)
	if err != nil || got.Limits != parent.Limits {
		t.Fatalf("ancestor limits widened: %+v err=%v", got.Limits, err)
	}
	child.Limits.MaxOutputTokens = 5
	got, err = ConstrainDelegation(parent, child)
	if err != nil || got.Limits.MaxOutputTokens != 5 {
		t.Fatalf("narrower child output limit lost: %+v %v", got.Limits, err)
	}
}

func TestSnapshotIsAnExplicitValidProfileMode(t *testing.T) {
	profile := validProfile("snapshot-worker")
	profile.WorkspaceMode = WorkspaceSnapshot
	if err := profile.Validate(); err != nil {
		t.Fatal(err)
	}
}
