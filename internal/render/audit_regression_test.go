package render

import (
	"strings"
	"testing"
)

func TestTextSanitizerBlocksSplitCommandsAndPreservesUnicode(t *testing.T) {
	var sanitizer TextSanitizer
	var output strings.Builder
	for _, part := range []string{"hello\x1b", "]52;c;", strings.Repeat("secret", 1000), "\x07世", "界\x1b[2", "J\x08!\n"} {
		output.WriteString(sanitizer.Filter(part))
	}
	if got := output.String(); got != "hello世界!\n" {
		t.Fatalf("untrusted commands passed through: %q", got)
	}
}

func TestSanitizerRetainsUTF8RuneAcrossByteChunks(t *testing.T) {
	input := "中🙂e\u0301"
	var sanitizer TextSanitizer
	var output strings.Builder
	for index := range len(input) {
		output.WriteString(sanitizer.Filter(input[index : index+1]))
	}
	if output.String() != input {
		t.Fatalf("split UTF-8 was damaged: %q", output.String())
	}
}

func TestStreamRendererBoundsAmbiguousFenceLine(t *testing.T) {
	var output strings.Builder
	renderer := newMarkdownStreamRenderer(&output, true)
	input := "```" + strings.Repeat("a", 100_000)
	for start := 0; start < len(input); start += 100 {
		if err := renderer.Write(input[start:min(start+100, len(input))]); err != nil {
			t.Fatal(err)
		}
		if len(renderer.pending) > maxMarkdownPendingBytes {
			t.Fatalf("pending grew to %d", len(renderer.pending))
		}
	}
	if err := renderer.Finish(); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); got != input {
		t.Fatal("bounded fallback lost or altered output")
	}
}

func TestPlainStreamNeverPassesControlCommands(t *testing.T) {
	var output strings.Builder
	renderer := newMarkdownStreamRenderer(&output, false)
	for _, part := range []string{"good\x1b]", "8;;https://evil.invalid\x1b\\label\x1b]8;;\x1b\\"} {
		if err := renderer.Write(part); err != nil {
			t.Fatal(err)
		}
	}
	if got := output.String(); got != "goodlabel" {
		t.Fatalf("stream control injection: %q", got)
	}
}
