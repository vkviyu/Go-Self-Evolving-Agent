package evaluation

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const maximumJSONLRecordBytes = 64 << 20

// LoadTasks reads JSONL task definitions and rejects invalid tasks or duplicate
// identities.
func LoadTasks(reader io.Reader) ([]Task, error) {
	if reader == nil {
		return nil, fmt.Errorf("task reader is required")
	}
	tasks := make([]Task, 0)
	seen := make(map[string]struct{})
	err := decodeJSONL(reader, "task", func(line int, task Task) error {
		if err := task.Validate(); err != nil {
			return fmt.Errorf("validate task line %d: %w", line, err)
		}
		if _, exists := seen[task.ID]; exists {
			return fmt.Errorf("duplicate task id %q", task.ID)
		}
		seen[task.ID] = struct{}{}
		tasks = append(tasks, task)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(tasks) == 0 {
		return nil, fmt.Errorf("task manifest is empty")
	}
	return tasks, nil
}

// LoadResults reads JSONL evaluation results and checks each record's schema and task ID.
func LoadResults(reader io.Reader) ([]Result, error) {
	if reader == nil {
		return nil, fmt.Errorf("result reader is required")
	}
	results := make([]Result, 0)
	err := decodeJSONL(reader, "result", func(line int, result Result) error {
		if result.Schema != ResultSchema || result.TaskID == "" {
			return fmt.Errorf("result line %d has an invalid schema or task_id", line)
		}
		results = append(results, result)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

// decodeJSONL strictly decodes one record per non-blank line and passes it to
// visit with its 1-based line number. kind names the record in errors.
func decodeJSONL[T any](reader io.Reader, kind string, visit func(int, T) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), maximumJSONLRecordBytes)
	for line := 1; scanner.Scan(); line++ {
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			continue
		}
		var record T
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&record); err != nil {
			return fmt.Errorf("decode %s line %d: %w", kind, line, err)
		}
		if err := requireJSONRecordEnd(decoder); err != nil {
			return fmt.Errorf("decode %s line %d: %w", kind, line, err)
		}
		if err := visit(line, record); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan %ss: %w", kind, err)
	}
	return nil
}

func requireJSONRecordEnd(decoder *json.Decoder) error {
	var trailing json.RawMessage
	err := decoder.Decode(&trailing)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("invalid trailing data: %w", err)
	}
	return fmt.Errorf("multiple JSON values in one JSONL record")
}

func WriteResult(writer io.Writer, result Result) error {
	if writer == nil {
		return fmt.Errorf("result writer is required")
	}
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(result)
}
