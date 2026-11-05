package workspacecoord

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	udiff "github.com/aymanbagabas/go-udiff"
)

const (
	conflictMainMarker      = "<<<<<<< gsea:main"
	conflictSeparator       = "======="
	conflictCandidatePrefix = ">>>>>>> gsea:candidate"
)

type (
	// Merge3Result is a line-level three-way merge. Text holds conflict
	// markers for every conflicted region; it is only written to a
	// resolution view, never to the main workspace.
	Merge3Result struct {
		Text      string
		Conflicts int
		Binary    bool
	}

	// lineHunk replaces base lines [start, end) with lines. An empty range is
	// an insertion before base line start.
	lineHunk struct {
		start int
		end   int
		lines []string
		ours  bool
	}
)

// Merge3 merges theirs (the candidate) into ours (current main) relative to
// base. Overlapping edits, and edits that start at the same base position,
// conflict unless both sides produce the same text there. Non-text content
// merges only when one side is unchanged.
func Merge3(base, ours, theirs, candidateLabel string) (Merge3Result, error) {
	switch {
	case ours == theirs || base == theirs:
		return Merge3Result{Text: ours}, nil
	case base == ours:
		return Merge3Result{Text: theirs}, nil
	case !isText(base) || !isText(ours) || !isText(theirs):
		return Merge3Result{Conflicts: 1, Binary: true}, nil
	}
	oursHunks, err := lineHunks(base, ours, true)
	if err != nil {
		return Merge3Result{}, err
	}
	theirHunks, err := lineHunks(base, theirs, false)
	if err != nil {
		return Merge3Result{}, err
	}
	all := append(oursHunks, theirHunks...)
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].start != all[j].start {
			return all[i].start < all[j].start
		}
		return all[i].end < all[j].end
	})
	baseLines := splitLines(base)
	var builder strings.Builder
	result := Merge3Result{}
	cursor := 0
	for _, group := range groupHunks(all) {
		start, end := groupRange(group)
		writeLines(&builder, baseLines[cursor:start])
		cursor = end
		region := baseLines[start:end]
		oursText := applyHunks(region, start, group, true)
		theirText := applyHunks(region, start, group, false)
		// A group edited by one side only takes that side's text.
		if side, single := singleSide(group); single {
			if side {
				builder.WriteString(oursText)
			} else {
				builder.WriteString(theirText)
			}
			continue
		}
		if oursText == theirText {
			builder.WriteString(oursText)
			continue
		}
		result.Conflicts++
		writeConflict(&builder, oursText, theirText, candidateLabel)
	}
	writeLines(&builder, baseLines[cursor:])
	result.Text = builder.String()
	return result, nil
}

// lineHunks converts the byte edits between base and other into whole-line
// hunks without context.
func lineHunks(base, other string, ours bool) ([]lineHunk, error) {
	edits := udiff.Strings(base, other)
	if len(edits) == 0 {
		return nil, nil
	}
	diff, err := udiff.ToUnifiedDiff("base", "other", base, edits, 0)
	if err != nil {
		return nil, fmt.Errorf("compute line diff: %w", err)
	}
	hunks := make([]lineHunk, 0, len(diff.Hunks))
	baseLines := splitLines(base)
	for _, hunk := range diff.Hunks {
		// Adjacent edits share one unified hunk. A delete after an insert
		// starts the next edit, so each edit keeps its own base range.
		item := lineHunk{start: hunk.FromLine - 1, end: hunk.FromLine - 1, ours: ours}
		previous := udiff.Equal
		for _, line := range hunk.Lines {
			if line.Kind != udiff.Insert && previous == udiff.Insert {
				hunks = appendTrimmed(hunks, item, baseLines)
				item = lineHunk{start: item.end, end: item.end, ours: ours}
			}
			switch line.Kind {
			case udiff.Delete:
				item.end++
			case udiff.Insert:
				item.lines = append(item.lines, line.Content)
			case udiff.Equal:
				// Context lines are kept as replacement text so the hunk
				// still covers a contiguous base range.
				item.end++
				item.lines = append(item.lines, line.Content)
			}
			previous = line.Kind
		}
		hunks = appendTrimmed(hunks, item, baseLines)
	}
	return hunks, nil
}

// appendTrimmed appends a hunk without the unchanged lines that widening
// byte edits to whole lines pulled into it. Hunks that change nothing are
// dropped.
func appendTrimmed(hunks []lineHunk, item lineHunk, baseLines []string) []lineHunk {
	for item.start < item.end && len(item.lines) > 0 && baseLines[item.start] == item.lines[0] {
		item.start++
		item.lines = item.lines[1:]
	}
	for item.start < item.end && len(item.lines) > 0 && baseLines[item.end-1] == item.lines[len(item.lines)-1] {
		item.end--
		item.lines = item.lines[:len(item.lines)-1]
	}
	if item.start == item.end && len(item.lines) == 0 {
		return hunks
	}
	// Edits that still touch after trimming form one logical change.
	if count := len(hunks); count > 0 && hunks[count-1].end == item.start {
		hunks[count-1].end = item.end
		hunks[count-1].lines = append(hunks[count-1].lines, item.lines...)
		return hunks
	}
	return append(hunks, item)
}

// groupHunks splits sorted hunks into groups that must be resolved together.
// A hunk joins the current group when it conflicts with any member, which
// makes overlap transitive across both sides.
func groupHunks(hunks []lineHunk) [][]lineHunk {
	var groups [][]lineHunk
	for _, hunk := range hunks {
		if count := len(groups); count > 0 && conflictsWithGroup(groups[count-1], hunk) {
			groups[count-1] = append(groups[count-1], hunk)
			continue
		}
		groups = append(groups, []lineHunk{hunk})
	}
	return groups
}

// conflictsWithGroup reports whether hunk overlaps or shares a start with a
// group member.
func conflictsWithGroup(group []lineHunk, hunk lineHunk) bool {
	for _, member := range group {
		if member.start == hunk.start || (member.start < hunk.end && hunk.start < member.end) {
			return true
		}
	}
	return false
}

// singleSide reports whether every hunk of a group comes from one side, and
// which side that is.
func singleSide(group []lineHunk) (bool, bool) {
	for _, hunk := range group[1:] {
		if hunk.ours != group[0].ours {
			return false, false
		}
	}
	return group[0].ours, true
}

// groupRange returns the base line range covered by a group.
func groupRange(group []lineHunk) (int, int) {
	start, end := group[0].start, group[0].end
	for _, hunk := range group[1:] {
		start = min(start, hunk.start)
		end = max(end, hunk.end)
	}
	return start, end
}

// applyHunks renders one side of a group over its base region.
func applyHunks(region []string, offset int, group []lineHunk, ours bool) string {
	var builder strings.Builder
	cursor := offset
	for _, hunk := range group {
		if hunk.ours != ours {
			continue
		}
		writeLines(&builder, region[cursor-offset:hunk.start-offset])
		writeLines(&builder, hunk.lines)
		cursor = hunk.end
	}
	writeLines(&builder, region[cursor-offset:])
	return builder.String()
}

// writeConflict writes one marked conflict region.
func writeConflict(builder *strings.Builder, ours, theirs, candidateLabel string) {
	builder.WriteString(conflictMainMarker + "\n")
	builder.WriteString(terminated(ours))
	builder.WriteString(conflictSeparator + "\n")
	builder.WriteString(terminated(theirs))
	builder.WriteString(strings.TrimSpace(conflictCandidatePrefix + " " + candidateLabel))
	builder.WriteString("\n")
}

// terminated adds a final newline so a marker always starts its own line.
func terminated(text string) string {
	if text != "" && !strings.HasSuffix(text, "\n") {
		return text + "\n"
	}
	return text
}

// writeLines appends lines that already carry their newlines.
func writeLines(builder *strings.Builder, lines []string) {
	for _, line := range lines {
		builder.WriteString(line)
	}
}

// splitLines splits text after each newline. The last line may lack one.
func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	lines := strings.SplitAfter(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// isText reports whether content is valid UTF-8 without NUL bytes.
func isText(content string) bool {
	return utf8.ValidString(content) && !strings.Contains(content, "\x00")
}

// HasConflictMarkers reports whether content still holds markers written by
// Merge3, which must never reach the main workspace.
func HasConflictMarkers(content string) bool {
	for _, line := range splitLines(content) {
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, conflictMainMarker) || strings.HasPrefix(line, conflictCandidatePrefix) {
			return true
		}
	}
	return false
}
