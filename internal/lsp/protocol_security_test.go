package lsp

import (
	"bufio"
	"strings"
	"testing"
)

func TestProtocolRejectsUnboundedOrDuplicateHeaders(t *testing.T) {
	for _, input := range []string{strings.Repeat("X", maxProtocolHeaderBytes+1), "Content-Length: 0\r\nContent-Length: 0\r\n\r\n"} {
		if _, err := readProtocolMessage(bufio.NewReader(strings.NewReader(input))); err == nil {
			t.Fatal("unsafe protocol header accepted")
		}
	}
}
