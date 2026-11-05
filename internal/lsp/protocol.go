package lsp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const maxProtocolMessageBytes = 8 << 20
const maxProtocolHeaderBytes = 16 << 10

type wireMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *wireError      `json:"error,omitempty"`
}

type wireError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *wireError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("LSP error %d: %s", e.Code, e.Message)
}

type protocolPosition struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type protocolRange struct {
	Start protocolPosition `json:"start"`
	End   protocolPosition `json:"end"`
}

type protocolDiagnostic struct {
	Range    protocolRange `json:"range"`
	Severity int           `json:"severity,omitempty"`
	Code     any           `json:"code,omitempty"`
	Source   string        `json:"source,omitempty"`
	Message  string        `json:"message"`
}

type publishDiagnosticsParams struct {
	URI         string               `json:"uri"`
	Version     *int                 `json:"version,omitempty"`
	Diagnostics []protocolDiagnostic `json:"diagnostics"`
}

func readProtocolMessage(reader *bufio.Reader) ([]byte, error) {
	contentLength := -1
	headerBytes := 0
	for {
		var fragments []byte
		for {
			fragment, readErr := reader.ReadSlice('\n')
			headerBytes += len(fragment)
			if headerBytes > maxProtocolHeaderBytes {
				return nil, errors.New("LSP headers exceed the size limit")
			}
			fragments = append(fragments, fragment...)
			if errors.Is(readErr, bufio.ErrBufferFull) {
				continue
			}
			if readErr != nil {
				return nil, readErr
			}
			break
		}
		line := string(fragments)
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("invalid LSP header %q", line)
		}
		if !strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			continue
		}
		parsed, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || parsed < 0 {
			return nil, fmt.Errorf("invalid LSP content length %q", value)
		}
		if contentLength >= 0 {
			return nil, errors.New("duplicate LSP Content-Length header")
		}
		contentLength = parsed
	}
	if contentLength < 0 {
		return nil, errors.New("LSP message has no Content-Length header")
	}
	if contentLength > maxProtocolMessageBytes {
		return nil, fmt.Errorf("LSP message is %d bytes; maximum is %d", contentLength, maxProtocolMessageBytes)
	}
	payload := make([]byte, contentLength)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func writeProtocolMessage(writer io.Writer, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode LSP message: %w", err)
	}
	if len(payload) > maxProtocolMessageBytes {
		return fmt.Errorf("LSP message is %d bytes; maximum is %d", len(payload), maxProtocolMessageBytes)
	}
	var framed bytes.Buffer
	fmt.Fprintf(&framed, "Content-Length: %d\r\n\r\n", len(payload))
	framed.Write(payload)
	if _, err := writer.Write(framed.Bytes()); err != nil {
		return fmt.Errorf("write LSP message: %w", err)
	}
	return nil
}

func wireIDKey(id json.RawMessage) string {
	return string(bytes.TrimSpace(id))
}
