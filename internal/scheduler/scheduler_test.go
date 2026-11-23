package scheduler

import "testing"

func TestBucketSetBoundsConcurrentRuns(t *testing.T) {
	set := NewBucketSet(1, map[string]int{"model-a": 2})
	first, second, third := set.TryAcquire("model-a"), set.TryAcquire("model-a"), set.TryAcquire("model-a")
	if !first || !second || third {
		t.Fatal("bucket did not enforce model limit")
	}
	set.Release("model-a")
	if !set.TryAcquire("model-a") {
		t.Fatal("released bucket slot was not reusable")
	}
	if !set.TryAcquire("other") || set.TryAcquire("other") {
		t.Fatal("bucket did not enforce default limit")
	}
}

func TestRoundRobinAdvancesTeamCursor(t *testing.T) {
	queue := &RoundRobin{}
	items := []Candidate{{TeamKey: "a", ItemKey: "a1", Priority: 1}, {TeamKey: "a", ItemKey: "a2", Priority: 5}, {TeamKey: "b", ItemKey: "b1", Priority: 1}}
	first := queue.Order(items)
	second := queue.Order(items)
	if first[0].TeamKey == second[0].TeamKey {
		t.Fatalf("team cursor did not advance: first=%#v second=%#v", first, second)
	}
	if first[0].ItemKey != "a2" {
		t.Fatalf("priority was not preserved inside team: %#v", first)
	}
}
