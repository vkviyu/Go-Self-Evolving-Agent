package render

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestMarkdownStreamRendererPreservesPlainOutput(t *testing.T) {
	var output strings.Builder
	renderer := newMarkdownStreamRenderer(&output, false)
	for _, chunk := range []string{"before\n```go\n", "const answer = 42\n", "```\nafter"} {
		if err := renderer.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := renderer.Finish(); err != nil {
		t.Fatal(err)
	}
	want := "before\n```go\nconst answer = 42\n```\nafter"
	if output.String() != want {
		t.Fatalf("output = %q, want %q", output.String(), want)
	}
}

func TestMarkdownStreamRendererWritesPlainDeltaImmediately(t *testing.T) {
	var output strings.Builder
	renderer := newMarkdownStreamRenderer(&output, true)
	if err := renderer.Write("streaming text"); err != nil {
		t.Fatal(err)
	}
	if output.String() != "streaming text" {
		t.Fatalf("output before newline = %q", output.String())
	}
}

func TestMarkdownStreamRendererWritesCodeDeltaImmediately(t *testing.T) {
	var output strings.Builder
	renderer := newMarkdownStreamRenderer(&output, true)
	if err := renderer.Write("```go\nfunc main() {"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ansiOutput(output.String()), "func main() {") {
		t.Fatalf("code was buffered instead of streamed: %q", output.String())
	}
}

func TestHighlightSourceUsesLanguageTokens(t *testing.T) {
	goCode := highlightSource("func main() {\n\treturn 42\n}\n", "go")
	zigCode := highlightSource("const name = \"Zig\";\n", "zig")
	javaCode := highlightSource("public class Main {\n    public static void main(String[] args) {}\n}\n", "java")
	cppCode := highlightSource("#include <iostream>\nint main() { return 0; }\n", "c++")
	if !strings.Contains(ansiOutput(goCode), "func main()") || !strings.Contains(ansiOutput(goCode), "42") || !hasANSI(goCode) {
		t.Fatalf("Go code was not highlighted: %q", goCode)
	}
	if !strings.Contains(ansiOutput(zigCode), "const name") || !strings.Contains(ansiOutput(zigCode), "\"Zig\"") || !hasANSI(zigCode) {
		t.Fatalf("Zig code was not highlighted: %q", zigCode)
	}
	if !strings.Contains(ansiOutput(javaCode), "public class Main") || !hasANSI(javaCode) {
		t.Fatalf("Java code was not highlighted: %q", javaCode)
	}
	if !strings.Contains(ansiOutput(cppCode), "#include <iostream>") || !hasANSI(cppCode) {
		t.Fatalf("C++ code was not highlighted: %q", cppCode)
	}
	phpCode := highlightSource("<?php\necho \"hello\";\n", "php")
	if !strings.Contains(ansiOutput(phpCode), "echo") || !strings.Contains(ansiOutput(phpCode), "\"hello\"") || !hasANSI(phpCode) {
		t.Fatalf("PHP code was not highlighted: %q", phpCode)
	}
}

func TestMarkdownStreamRendererHighlightsClosedFence(t *testing.T) {
	var output strings.Builder
	renderer := newMarkdownStreamRenderer(&output, true)
	if err := renderer.Write("```zig\nconst value = 1;\n```\n"); err != nil {
		t.Fatal(err)
	}
	if err := renderer.Finish(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ansiOutput(output.String()), "const value") || !hasANSI(output.String()) {
		t.Fatalf("highlighted output = %q", output.String())
	}
}

func TestMarkdownStreamRendererNormalizesInlineOpeningFence(t *testing.T) {
	var output strings.Builder
	renderer := newMarkdownStreamRenderer(&output, true)
	for _, chunk := range []string{"给你一段代码。```go", "\npackage main\nfunc main() {}\n"} {
		if err := renderer.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := renderer.Finish(); err != nil {
		t.Fatal(err)
	}
	plain := ansiOutput(output.String())
	if !strings.Contains(plain, "给你一段代码。\n```go") || !strings.Contains(plain, "package main") || !hasANSI(output.String()) {
		t.Fatalf("inline fenced stream was not normalized/highlighted: %q", output.String())
	}
}

func hasANSI(value string) bool {
	return strings.Contains(value, "\x1b[")
}

func ansiOutput(value string) string {
	return ansi.Strip(value)
}

func TestParseFenceAliases(t *testing.T) {
	marker, language, ok := parseFenceOpening("```golang")
	if !ok || marker != '`' || language != "golang" {
		t.Fatalf("parsed fence = %q, %q, %v", marker, language, ok)
	}
	if NormalizeCodeLanguage(language) != "go" {
		t.Fatalf("normalized language = %q", NormalizeCodeLanguage(language))
	}
}
