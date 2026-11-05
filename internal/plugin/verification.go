package plugin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

type TestCase struct {
	Name        string          `json:"name"`
	Input       json.RawMessage `json:"input"`
	Expected    json.RawMessage `json:"expected,omitempty"`
	ExpectError bool            `json:"expect_error,omitempty"`
}

type CaseResult struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Error  string `json:"error,omitempty"`
}

type TestReport struct {
	Tool            string       `json:"tool"`
	Revision        string       `json:"revision"`
	ContentSHA256   string       `json:"content_sha256"`
	RuntimeIdentity string       `json:"runtime_identity"`
	CasesSHA256     string       `json:"cases_sha256"`
	Passed          bool         `json:"passed"`
	Results         []CaseResult `json:"results"`
	TestedAt        time.Time    `json:"tested_at"`
}

func ValidateCases(cases []TestCase) error {
	if len(cases) < 1 || len(cases) > 32 {
		return errors.New("plugin verification requires 1 to 32 cases")
	}
	seen := map[string]bool{}
	for _, item := range cases {
		if item.Name == "" || len(item.Name) > 128 || seen[item.Name] {
			return errors.New("test names must be unique and bounded")
		}
		seen[item.Name] = true
		if len(item.Input) > 16384 || !json.Valid(item.Input) || len(item.Expected) > 16384 {
			return errors.New("test JSON is invalid or exceeds 16 KiB")
		}
		if item.ExpectError {
			if len(item.Expected) > 0 {
				return errors.New("error cases cannot also expect a result")
			}
		} else if !json.Valid(item.Expected) {
			return errors.New("test requires expected JSON or expect_error")
		}
	}
	return nil
}

// Verify exercises only a fresh WASM guest, which has no host filesystem or
// network capabilities. Passing these explicit cases is evidence, not a proof
// of general safety and never promotion authority.
func (m *Manager) Verify(ctx context.Context, name, revision string, cases []TestCase) (TestReport, error) {
	var report TestReport
	if err := ValidateCases(cases); err != nil {
		return report, err
	}
	m.mu.Lock()
	if err := m.mutationError(); err != nil {
		m.mu.Unlock()
		return report, err
	}
	artifact := m.artifacts[artifactKey(name, revision)]
	m.mu.Unlock()
	if artifact == nil {
		return report, errors.New("plugin revision not found")
	}
	encoded, _ := json.Marshal(cases)
	sum := sha256.Sum256(encoded)
	report = TestReport{Tool: name, Revision: revision, ContentSHA256: artifact.contentSHA, RuntimeIdentity: artifact.runtimeIdentity, CasesSHA256: hex.EncodeToString(sum[:]), Passed: true, TestedAt: time.Now().UTC()}
	testCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for _, item := range cases {
		if err := testCtx.Err(); err != nil {
			return report, err
		}
		actual, err := artifact.module.Invoke(testCtx, item.Input)
		if testCtx.Err() != nil {
			report.Passed = false
			return report, testCtx.Err()
		}
		result := CaseResult{Name: item.Name}
		if item.ExpectError {
			result.Passed = err != nil
		} else {
			result.Passed = err == nil && equalJSON(actual, item.Expected)
		}
		if !result.Passed {
			result.Error = "result did not match expectation"
			if err != nil {
				result.Error = fmt.Sprintf("guest failed: %.1024s", err.Error())
			}
			report.Passed = false
		}
		report.Results = append(report.Results, result)
	}
	return report, nil
}

func equalJSON(left, right []byte) bool {
	canonical := func(raw []byte) ([]byte, error) {
		var value any
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
			return nil, errors.New("expected one JSON result")
		}
		return json.Marshal(value)
	}
	a, err := canonical(left)
	if err != nil {
		return false
	}
	b, err := canonical(right)
	return err == nil && bytes.Equal(a, b)
}
