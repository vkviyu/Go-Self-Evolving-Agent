package scheduler

import "testing"

func TestReleaseReportsUnmatchedSlot(t *testing.T) {
	set := NewBucketSet(1, nil)
	if set.Release("missing") {
		t.Fatal("unmatched release reported success")
	}
	if !set.TryAcquire("m") || !set.Release("m") || set.Release("m") {
		t.Fatal("acquire/release identity was not tracked")
	}
}
