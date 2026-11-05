package workspacecoord

import (
	"strings"
	"testing"
)

func TestMerge3CombinesDisjointEdits(t *testing.T) {
	base := "a\nb\nc\nd\ne\n"
	ours := "A\nb\nc\nd\ne\n"
	theirs := "a\nb\nc\nd\nE\n"
	result, err := Merge3(base, ours, theirs, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Conflicts != 0 || result.Text != "A\nb\nc\nd\nE\n" {
		t.Fatalf("result = %+v", result)
	}
}

func TestMerge3MergesAdjacentInsertAfterReplacement(t *testing.T) {
	base := "a\nb\nc\n"
	ours := "a\nB\nc\n"
	theirs := "a\nb\nx\nc\n"
	result, err := Merge3(base, ours, theirs, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Conflicts != 0 || result.Text != "a\nB\nx\nc\n" {
		t.Fatalf("result = %+v", result)
	}
}

func TestMerge3MarksOverlappingEdits(t *testing.T) {
	base := "a\nb\nc\n"
	ours := "a\nmain\nc\n"
	theirs := "a\ncandidate\nc\n"
	result, err := Merge3(base, ours, theirs, "m1")
	if err != nil {
		t.Fatal(err)
	}
	want := "a\n<<<<<<< gsea:main\nmain\n=======\ncandidate\n>>>>>>> gsea:candidate m1\nc\n"
	if result.Conflicts != 1 || result.Text != want {
		t.Fatalf("result = %+v\nwant %q", result, want)
	}
	if !HasConflictMarkers(result.Text) {
		t.Fatal("markers were not detected")
	}
}

func TestMerge3ConflictsOnDifferentInsertionsAtSamePosition(t *testing.T) {
	base := "a\nb\n"
	result, err := Merge3(base, "a\nx\nb\n", "a\ny\nb\n", "m1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Conflicts != 1 || strings.Contains(result.Text, "x\ny\n") {
		t.Fatalf("insertions were concatenated: %+v", result)
	}
}

func TestMerge3DeduplicatesIdenticalEdits(t *testing.T) {
	base := "a\nb\nc\n"
	both := "a\nB\nc\n"
	result, err := Merge3(base, both, "a\nB\nc\nd\n", "m1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Conflicts != 0 || result.Text != "a\nB\nc\nd\n" {
		t.Fatalf("result = %+v", result)
	}
}

func TestMerge3GroupsTransitiveOverlap(t *testing.T) {
	base := "1\n2\n3\n4\n5\n"
	ours := "1\nX\nX\n4\n5\n"
	theirs := "1\n2\nY\nY\n5\n"
	result, err := Merge3(base, ours, theirs, "m1")
	if err != nil {
		t.Fatal(err)
	}
	want := "1\n<<<<<<< gsea:main\nX\nX\n4\n=======\n2\nY\nY\n>>>>>>> gsea:candidate m1\n5\n"
	if result.Conflicts != 1 || result.Text != want {
		t.Fatalf("result = %q", result.Text)
	}
}

func TestMerge3RejectsBinaryConflict(t *testing.T) {
	result, err := Merge3("a\x00", "b\x00", "c\x00", "m1")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Binary || result.Conflicts != 1 {
		t.Fatalf("result = %+v", result)
	}
	trivial, err := Merge3("a\x00", "a\x00", "c\x00", "m1")
	if err != nil || trivial.Conflicts != 0 || trivial.Text != "c\x00" {
		t.Fatalf("trivial = %+v, %v", trivial, err)
	}
}

func TestMerge3HandlesMissingFinalNewline(t *testing.T) {
	base := "a\nb"
	result, err := Merge3(base, "A\nb", "a\nb\nc", "m1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Conflicts != 0 || result.Text != "A\nb\nc" {
		t.Fatalf("result = %q", result.Text)
	}
}
