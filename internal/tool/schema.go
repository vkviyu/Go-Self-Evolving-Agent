package tool

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

const maxArgumentBytes = 16 << 20

type noSchemaFetch struct{}

func (noSchemaFetch) Load(url string) (any, error) {
	return nil, fmt.Errorf("external schema references are disabled: %s", url)
}

type compiledInput struct {
	once   sync.Once
	schema *jsonschema.Schema
	err    error
}

func compileInputSchema(raw json.RawMessage) (*jsonschema.Schema, error) {
	if len(raw) > MaxSnapshotManifestBytes {
		return nil, errors.New("tool input schema is too large")
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseLoader(noSchemaFetch{})
	const location = "https://gsea.invalid/tool-input.json"
	if err := compiler.AddResource(location, document); err != nil {
		return nil, err
	}
	return compiler.Compile(location)
}

func (snapshot *Snapshot) validateArguments(binding Binding, raw json.RawMessage) error {
	if len(raw) > maxArgumentBytes {
		return &PreconditionError{Code: "invalid_arguments", Message: "tool arguments exceed the size limit"}
	}
	value, _ := snapshot.inputSchemas.LoadOrStore(binding.Descriptor.Name, &compiledInput{})
	compiled := value.(*compiledInput)
	compiled.once.Do(func() { compiled.schema, compiled.err = compileInputSchema(binding.Descriptor.InputSchema) })
	if compiled.err != nil {
		return fmt.Errorf("compile input schema for %s: %w", binding.Descriptor.Name, compiled.err)
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return &PreconditionError{Code: "invalid_arguments", Message: "tool arguments must contain one JSON value"}
	}
	if err := compiled.schema.Validate(instance); err != nil {
		message := err.Error()
		if len(message) > 2048 {
			message = message[:2048] + "…"
		}
		return &PreconditionError{Code: "invalid_arguments", Message: message}
	}
	return nil
}
