package toolbridge

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAndInvokeBridge(t *testing.T) {
	directory := t.TempDir()
	manifestPath := filepath.Join(directory, "manifest.json")
	socketPath := filepath.Join(directory, "bridge.sock")
	manifest := `{"schema":"gsea.tool-bridge.manifest.v1","tools":[{"name":"lookup_item","description":"Look up an item.","input_schema":{"type":"object","additionalProperties":false,"required":["id"],"properties":{"id":{"type":"string"}}}}]}`
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			done <- acceptErr
			return
		}
		defer connection.Close()
		var request callRequest
		if decodeErr := json.NewDecoder(bufio.NewReader(connection)).Decode(&request); decodeErr != nil {
			done <- decodeErr
			return
		}
		done <- json.NewEncoder(connection).Encode(callResult{
			Schema: ResultSchema, CallID: request.CallID,
			Result: json.RawMessage(`{"name":"example"}`),
		})
	}()
	bindings, err := Load(manifestPath, socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 || bindings[0].Descriptor.Name != "lookup_item" {
		t.Fatalf("bindings = %#v", bindings)
	}
	result, err := bindings[0].Executor.Invoke(context.Background(), json.RawMessage(`{"id":"7"}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(result) != `{"name":"example"}` {
		t.Fatalf("result = %s", result)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestLoadRejectsInvalidBoundary(t *testing.T) {
	directory := t.TempDir()
	manifestPath := filepath.Join(directory, "manifest.json")
	if err := os.WriteFile(manifestPath, []byte(`{"schema":"wrong","tools":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(manifestPath, "relative.sock"); err == nil {
		t.Fatal("relative socket path was accepted")
	}
	if _, err := Load(manifestPath, filepath.Join(directory, "bridge.sock")); err == nil {
		t.Fatal("invalid manifest schema was accepted")
	}
}
