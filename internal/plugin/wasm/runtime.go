// Package wasm runs untrusted tool revisions in isolated wazero instances.
package wasm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

const (
	hostModuleName       = "local_agent_host_v1"
	pluginABIVersionName = "plugin_abi_version"
	pluginInvokeName     = "plugin_invoke"
	supportedABIVersion  = uint32(1)
	defaultMemoryPages   = uint32(1024) // 64 MiB
	defaultMaxBlobBytes  = uint64(16 << 20)
	defaultMaxCallBytes  = uint64(32 << 20)
	defaultMaxCalls      = 4
	maxGuestErrorBytes   = 4096
)

var ErrRuntimeClosed = errors.New("WASM runtime is closed")

var allowedHostImports = map[string]struct{}{
	"host_blob_len":  {},
	"host_blob_read": {},
	"host_blob_put":  {},
	"host_blob_drop": {},
}

// wazero v1.12.0 lazily initializes internal build-version state without
// synchronization. Serialize construction so concurrent App.Open calls cannot
// race inside that third-party one-time path.
var runtimeConstructionMutex sync.Mutex

// Config bounds memory, transfer and execution resources for all plugins.
type Config struct {
	MemoryLimitPages uint32
	MaxBlobBytes     uint64
	MaxCallBytes     uint64
	DefaultTimeout   time.Duration
	// MaxConcurrentCalls bounds the aggregate live guest-memory and retained
	// host-blob allocations. It does not bound compiled code or host GC RSS.
	MaxConcurrentCalls int
}

// Revision is immutable plugin identity and its call-level timeout.
type Revision struct {
	PluginID string
	Revision string
	Timeout  time.Duration
}

// Runtime owns compiled WASM code and the host ABI. It is safe for concurrent
// calls; every invocation receives a fresh isolated module instance.
type Runtime struct {
	runtime          wazero.Runtime
	blobs            *blobStore
	defaultTimeout   time.Duration
	resourceIdentity string
	nextCall         atomic.Uint64
	slots            chan struct{}
	done             chan struct{}
	closeOnce        sync.Once
}

// NewRuntime creates the wazero engine and instantiates WASI plus the narrow
// local-agent host ABI. No host filesystem or environment is preopened.
func NewRuntime(ctx context.Context, config Config) (*Runtime, error) {
	if config.MemoryLimitPages == 0 {
		config.MemoryLimitPages = defaultMemoryPages
	}
	if config.MemoryLimitPages > 65536 {
		return nil, errors.New("WASM memory page limit exceeds the wasm32 address space")
	}
	if config.MaxBlobBytes == 0 {
		config.MaxBlobBytes = defaultMaxBlobBytes
	}
	if config.MaxCallBytes == 0 {
		config.MaxCallBytes = defaultMaxCallBytes
	}
	if config.MaxCallBytes < config.MaxBlobBytes {
		return nil, errors.New("WASM max call bytes must be at least max blob bytes")
	}
	if config.DefaultTimeout <= 0 {
		config.DefaultTimeout = 30 * time.Second
	}
	if config.MaxConcurrentCalls == 0 {
		config.MaxConcurrentCalls = defaultMaxCalls
	}
	if config.MaxConcurrentCalls < 1 || config.MaxConcurrentCalls > 64 {
		return nil, errors.New("WASM concurrent call limit must be between 1 and 64")
	}

	runtimeConstructionMutex.Lock()
	engine := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigCompiler().
		WithMemoryLimitPages(config.MemoryLimitPages).
		WithCloseOnContextDone(true))
	runtimeConstructionMutex.Unlock()
	runtime := &Runtime{
		runtime:          engine,
		blobs:            newBlobStore(config.MaxBlobBytes, config.MaxCallBytes),
		defaultTimeout:   config.DefaultTimeout,
		resourceIdentity: fmt.Sprintf("wasm-abi1:memory:%d:blob:%d:call:%d:timeout:%d:concurrency:%d", config.MemoryLimitPages, config.MaxBlobBytes, config.MaxCallBytes, config.DefaultTimeout, config.MaxConcurrentCalls),
		slots:            make(chan struct{}, config.MaxConcurrentCalls),
		done:             make(chan struct{}),
	}
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, engine); err != nil {
		_ = engine.Close(ctx)
		return nil, fmt.Errorf("instantiate WASI: %w", err)
	}
	if err := runtime.instantiateHost(ctx); err != nil {
		_ = engine.Close(ctx)
		return nil, err
	}
	return runtime, nil
}

// ResourceIdentity binds the normalized sandbox limits, not a local path.
func (r *Runtime) ResourceIdentity() string { return r.resourceIdentity }

func (r *Runtime) DefaultTimeout() time.Duration { return r.defaultTimeout }

func (r *Runtime) instantiateHost(ctx context.Context) error {
	builder := r.runtime.NewHostModuleBuilder(hostModuleName)
	builder.NewFunctionBuilder().WithFunc(r.hostBlobLen).Export("host_blob_len")
	builder.NewFunctionBuilder().WithFunc(r.hostBlobRead).Export("host_blob_read")
	builder.NewFunctionBuilder().WithFunc(r.hostBlobPut).Export("host_blob_put")
	builder.NewFunctionBuilder().WithFunc(r.hostBlobDrop).Export("host_blob_drop")
	if _, err := builder.Instantiate(ctx); err != nil {
		return fmt.Errorf("instantiate local-agent WASM host ABI: %w", err)
	}
	return nil
}

// Compile validates an immutable WASM artifact before it can enter a registry
// snapshot. The compiled module is reusable; instances are not.
func (r *Runtime) Compile(ctx context.Context, identity Revision, source []byte) (*Module, error) {
	select {
	case <-r.done:
		return nil, ErrRuntimeClosed
	default:
	}
	if identity.PluginID == "" || identity.Revision == "" {
		return nil, errors.New("WASM plugin ID and revision are required")
	}
	if len(source) == 0 {
		return nil, errors.New("WASM source is empty")
	}
	compiled, err := r.runtime.CompileModule(ctx, source)
	if err != nil {
		return nil, fmt.Errorf("compile WASM %s@%s: %w", identity.PluginID, identity.Revision, err)
	}
	if err := validateModule(compiled); err != nil {
		_ = compiled.Close(ctx)
		return nil, fmt.Errorf("validate WASM %s@%s: %w", identity.PluginID, identity.Revision, err)
	}
	return &Module{owner: r, identity: identity, compiled: compiled}, nil
}

// Module is one compiled immutable plugin revision.
type Module struct {
	owner    *Runtime
	identity Revision
	compiled wazero.CompiledModule
}

// Invoke executes one call in a fresh module instance and unwraps the SDK's
// response envelope. Context cancellation and timeout forcibly close it.
func (m *Module) Invoke(ctx context.Context, request json.RawMessage) (json.RawMessage, error) {
	timeout := m.identity.Timeout
	if timeout <= 0 {
		timeout = m.owner.defaultTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if uint64(len(request)) > m.owner.blobs.maxBlobBytes {
		return nil, errBlobLimit
	}
	if len(request) == 0 || !json.Valid(request) {
		return nil, errors.New("WASM request must be valid JSON")
	}
	if err := m.owner.acquire(callCtx); err != nil {
		return nil, fmt.Errorf("admit WASM call: %w", err)
	}
	defer func() { <-m.owner.slots }()

	callKey := fmt.Sprintf("local-agent-call-%d", m.owner.nextCall.Add(1))
	requestHandle, err := m.owner.blobs.begin(callKey, request)
	if err != nil {
		return nil, fmt.Errorf("create WASM request blob: %w", err)
	}
	defer m.owner.blobs.end(callKey)

	instance, err := m.owner.runtime.InstantiateModule(callCtx, m.compiled, wazero.NewModuleConfig().
		WithName(callKey).
		WithStartFunctions())
	if err != nil {
		return nil, fmt.Errorf("instantiate WASM %s@%s: %w", m.identity.PluginID, m.identity.Revision, err)
	}
	defer instance.Close(context.Background())

	if initialize := instance.ExportedFunction("_initialize"); initialize != nil {
		if _, err := initialize.Call(callCtx); err != nil {
			return nil, fmt.Errorf("initialize WASM %s@%s: %w", m.identity.PluginID, m.identity.Revision, err)
		}
	}
	version, err := instance.ExportedFunction(pluginABIVersionName).Call(callCtx)
	if err != nil {
		return nil, fmt.Errorf("read WASM ABI version: %w", err)
	}
	if len(version) != 1 || uint32(version[0]) != supportedABIVersion {
		return nil, fmt.Errorf("unsupported WASM ABI version %d", firstResult(version))
	}
	response, err := instance.ExportedFunction(pluginInvokeName).Call(callCtx, requestHandle)
	if err != nil {
		return nil, fmt.Errorf("invoke WASM %s@%s: %w", m.identity.PluginID, m.identity.Revision, err)
	}
	if len(response) != 1 || response[0] == 0 {
		return nil, errors.New("WASM plugin returned an invalid response handle")
	}
	payload, err := m.owner.blobs.getGuest(callKey, response[0])
	if err != nil {
		return nil, fmt.Errorf("read WASM response blob: %w", err)
	}
	// getGuest made a private copy. The SDK handed ownership of its returned
	// guest-owned blob to us, so release it before JSON decoding/copying.
	if err := m.owner.blobs.dropGuest(callKey, response[0]); err != nil {
		return nil, fmt.Errorf("release WASM response blob: %w", err)
	}
	var envelope responseEnvelope
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, fmt.Errorf("decode WASM response envelope: %w", err)
	}
	if !envelope.OK {
		if envelope.Error == "" {
			envelope.Error = "plugin failed without an error message"
		}
		return nil, errors.New(boundedGuestError(envelope.Error))
	}
	if len(envelope.Result) == 0 || !json.Valid(envelope.Result) {
		return nil, errors.New("WASM response contains invalid result JSON")
	}
	return append(json.RawMessage(nil), envelope.Result...), nil
}

func boundedGuestError(message string) string {
	if len(message) <= maxGuestErrorBytes {
		return message
	}
	return strings.ToValidUTF8(message[:maxGuestErrorBytes], "") + " [truncated]"
}

func (r *Runtime) acquire(ctx context.Context) error {
	select {
	case <-r.done:
		return ErrRuntimeClosed
	case <-ctx.Done():
		return ctx.Err()
	case r.slots <- struct{}{}:
	}
	select {
	case <-r.done:
		<-r.slots
		return ErrRuntimeClosed
	case <-ctx.Done():
		<-r.slots
		return ctx.Err()
	default:
		return nil
	}
}

type responseEnvelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  string          `json:"error"`
}

// Close releases this revision after no registry snapshot can reference it.
func (m *Module) Close(ctx context.Context) error {
	if err := m.compiled.Close(ctx); err != nil {
		return fmt.Errorf("close compiled WASM %s@%s: %w", m.identity.PluginID, m.identity.Revision, err)
	}
	return nil
}

// Close releases the runtime and all compiled/instantiated resources.
func (r *Runtime) Close(ctx context.Context) error {
	r.closeOnce.Do(func() { close(r.done) })
	if err := r.runtime.Close(ctx); err != nil {
		return fmt.Errorf("close WASM runtime: %w", err)
	}
	return nil
}

func validateModule(compiled wazero.CompiledModule) error {
	for _, definition := range compiled.ImportedFunctions() {
		moduleName, name, imported := definition.Import()
		if !imported {
			continue
		}
		switch moduleName {
		case wasi_snapshot_preview1.ModuleName:
			// WASI functions are provided without filesystem preopens.
		case hostModuleName:
			if _, allowed := allowedHostImports[name]; !allowed {
				return fmt.Errorf("host import %q is not in ABI v1", name)
			}
		default:
			return fmt.Errorf("import module %q is not allowed", moduleName)
		}
	}
	if len(compiled.ImportedMemories()) != 0 {
		return errors.New("imported memory is not allowed")
	}
	exports := compiled.ExportedFunctions()
	if err := checkSignature(exports[pluginABIVersionName], nil, []api.ValueType{api.ValueTypeI32}); err != nil {
		return fmt.Errorf("%s: %w", pluginABIVersionName, err)
	}
	if err := checkSignature(exports[pluginInvokeName], []api.ValueType{api.ValueTypeI64}, []api.ValueType{api.ValueTypeI64}); err != nil {
		return fmt.Errorf("%s: %w", pluginInvokeName, err)
	}
	return nil
}

func checkSignature(definition api.FunctionDefinition, parameters, results []api.ValueType) error {
	if definition == nil {
		return errors.New("required export is missing")
	}
	if !equalTypes(definition.ParamTypes(), parameters) || !equalTypes(definition.ResultTypes(), results) {
		return fmt.Errorf("unexpected signature params=%v results=%v", definition.ParamTypes(), definition.ResultTypes())
	}
	return nil
}

func equalTypes(left, right []api.ValueType) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func firstResult(results []uint64) uint64 {
	if len(results) == 0 {
		return 0
	}
	return results[0]
}

func (r *Runtime) hostBlobLen(_ context.Context, module api.Module, handle uint64) int64 {
	length, err := r.blobs.length(module.Name(), handle)
	if err != nil || length > uint64(^uint32(0)) {
		return -1
	}
	return int64(length)
}

func (r *Runtime) hostBlobRead(_ context.Context, module api.Module, handle uint64, destination, capacity uint32) int32 {
	data, err := r.blobs.get(module.Name(), handle)
	if err != nil || uint64(capacity) < uint64(len(data)) || !module.Memory().Write(destination, data) {
		return 1
	}
	return 0
}

func (r *Runtime) hostBlobPut(_ context.Context, module api.Module, source, length uint32) uint64 {
	data, ok := module.Memory().Read(source, length)
	if !ok {
		return 0
	}
	handle, err := r.blobs.putGuest(module.Name(), data)
	if err != nil {
		return 0
	}
	return handle
}

func (r *Runtime) hostBlobDrop(_ context.Context, module api.Module, handle uint64) int32 {
	if err := r.blobs.dropGuest(module.Name(), handle); err != nil {
		return 1
	}
	return 0
}
