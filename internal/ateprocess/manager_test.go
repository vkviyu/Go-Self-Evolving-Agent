//go:build darwin || linux

package ateprocess

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/agent-temporal-engine/model"
	"github.com/vkviyu/agent-temporal-engine/protocol"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimeowner"
)

type (
	launchRecord struct {
		PID   int
		Token string
		Args  []string
	}
)

func TestMain(m *testing.M) {
	if os.Getenv("GSEA_ATE_TEST_HELPER") != "" && len(os.Args) > 2 {
		switch os.Args[1] {
		case "init-state", "serve", "hold-lock":
			os.Exit(runHelper())
		}
	}
	os.Exit(m.Run())
}

// runHelper implements actual framed stdio in a child process, with explicit
// failure modes that are difficult to induce reliably in the real ATE binary.
func runHelper() int {
	mode := os.Getenv("GSEA_ATE_TEST_HELPER")
	directory := os.Args[len(os.Args)-1]
	if os.Args[1] == "hold-lock" {
		lock, err := runtimeowner.Acquire(directory)
		if err != nil {
			return 2
		}
		defer lock.Close()
		_, _ = os.Stdout.Write([]byte{1})
		_, _ = io.Copy(io.Discard, os.Stdin)
		return 0
	}
	record := launchRecord{PID: os.Getpid(), Token: os.Getenv(tokenEnvironment), Args: os.Args[1:]}
	data, _ := json.Marshal(record)
	suffix := ".launch"
	if os.Args[1] == "init-state" {
		suffix = ".init"
	}
	if err := os.WriteFile(directory+suffix, data, 0600); err != nil {
		return 2
	}
	if os.Args[1] == "init-state" {
		entries, err := os.ReadDir(directory)
		if err != nil || len(entries) != 0 {
			return 3
		}
		if mode == "init-failure" {
			fmt.Fprint(os.Stderr, "private stderr must not escape")
			return 4
		}
		if mode == "init-hang" {
			signal.Ignore(os.Interrupt)
			time.Sleep(time.Hour)
		}
		if err := os.WriteFile(filepath.Join(directory, "initialized"), []byte("ready"), 0600); err != nil {
			return 5
		}
		return 0
	}
	if mode == "hello-hang" {
		time.Sleep(time.Hour)
	}
	frame, err := protocol.ReadFrame(os.Stdin)
	if err != nil {
		return 6
	}
	request, err := protocol.DecodeRequest(frame)
	if err != nil || request.Method != "hello" {
		return 7
	}
	var hello protocol.HelloRequest
	if json.Unmarshal(request.Body, &hello) != nil || hello.Token != record.Token || len(record.Token) != 64 {
		return 8
	}
	identity := protocol.HelloResult{Principal: "gsea", AllSpaces: true,
		Capabilities: []string{"agent/v1", "event/v1", "knowledge/v1", "memory/v1", "memory/query-v1", "space/catalog-v1", "space/organization-v1"}}
	if strings.HasPrefix(mode, "missing:") {
		missing := strings.TrimPrefix(mode, "missing:")
		for i, capability := range identity.Capabilities {
			if capability == missing {
				identity.Capabilities = append(identity.Capabilities[:i], identity.Capabilities[i+1:]...)
				break
			}
		}
	}
	if mode == "restricted" {
		identity.AllSpaces, identity.Spaces = false, []model.SpaceID{"one"}
	}
	body, _ := json.Marshal(identity)
	response := protocol.Response{Version: protocol.Version, CorrelationID: request.CorrelationID, Body: body}
	if mode == "secret-error" {
		fmt.Fprint(os.Stderr, strings.Repeat(record.Token, 32768))
		response.Error = &protocol.Error{Code: api.ErrorUnauthorized, Message: record.Token + " private stderr must not escape"}
	}
	data, _ = json.Marshal(response)
	if protocol.WriteFrame(os.Stdout, data) != nil {
		return 9
	}
	if mode == "disconnect" {
		_, _ = protocol.ReadFrame(os.Stdin)
		return 0
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	if mode == "stubborn" {
		// Record whether another process could obtain the lock before this child
		// exited. The manager must keep it through its forced-kill fallback.
		lock, err := runtimeowner.Acquire(directory)
		if err == nil {
			lock.Close()
			_ = os.WriteFile(directory+".early-release", []byte("unsafe"), 0600)
		}
		time.Sleep(time.Hour)
	}
	return 0
}

func helperManager(t *testing.T, mode string, timeout time.Duration) *Manager {
	t.Helper()
	t.Setenv("GSEA_ATE_TEST_HELPER", mode)
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(Options{Binary: binary, Directory: filepath.Join(t.TempDir(), "temporal"), Timeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func readLaunch(t *testing.T, path string) launchRecord {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record launchRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	return record
}

func assertReapedAndUnlocked(t *testing.T, m *Manager, suffix string) {
	t.Helper()
	record := readLaunch(t, m.options.Directory+suffix)
	if err := syscall.Kill(record.PID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("child %d is still alive or unreaped: %v", record.PID, err)
	}
	lock, err := runtimeowner.Acquire(m.options.Directory)
	if err != nil {
		t.Fatalf("ownership not released: %v", err)
	}
	lock.Close()
}

func TestFreshCredentialsAndCallbackErrors(t *testing.T) {
	m := helperManager(t, "normal", 3*time.Second)
	if _, err := os.Stat(m.options.Directory); !os.IsNotExist(err) {
		t.Fatalf("New created the store: %v", err)
	}
	sentinel := errors.New("caller error")
	var first string
	for i := 0; i < 2; i++ {
		calls := 0
		err := m.WithClient(context.Background(), func(c *client.Client) error {
			calls++
			if c.Identity().Principal != "gsea" {
				return errors.New("wrong principal")
			}
			return sentinel
		})
		if !errors.Is(err, sentinel) || calls != 1 {
			t.Fatalf("callback calls=%d err=%v", calls, err)
		}
		record := readLaunch(t, m.options.Directory+".launch")
		if len(record.Token) != 64 || strings.Contains(strings.Join(record.Args, " "), record.Token) {
			t.Fatal("credential missing or exposed in arguments")
		}
		if i == 1 && record.Token == first {
			t.Fatal("credential reused across launches")
		}
		first = record.Token
		assertReapedAndUnlocked(t, m, ".launch")
	}
}

func TestPersistentLeaseReusesClientAndProcess(t *testing.T) {
	m := helperManager(t, "normal", 3*time.Second)
	lease, err := m.OpenClient(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var first *client.Client
	for i := 0; i < 3; i++ {
		if err := lease.WithClient(context.Background(), func(c *client.Client) error {
			if first == nil {
				first = c
			} else if first != c {
				return errors.New("persistent lease returned a different client")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	record := readLaunch(t, m.options.Directory+".launch")
	if err := syscall.Kill(record.PID, 0); err != nil {
		t.Fatalf("persistent child exited before lease close: %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	assertReapedAndUnlocked(t, m, ".launch")
	if err := lease.WithClient(context.Background(), func(*client.Client) error {
		return errors.New("closed lease callback invoked")
	}); !errors.Is(err, ErrLeaseClosed) {
		t.Fatalf("closed lease accepted callback: %v", err)
	}
}

func TestFreshClientReleasesInvalidLeaseBeforeRecovery(t *testing.T) {
	m := helperManager(t, "normal", 3*time.Second)
	lease, err := m.OpenClient(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	first := readLaunch(t, m.options.Directory+".launch")
	lease.Invalidate()
	var recovered *client.Client
	if err := m.WithFreshClient(context.Background(), func(c *client.Client) error {
		recovered = c
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if recovered == nil {
		t.Fatal("fresh callback did not receive a client")
	}
	second := readLaunch(t, m.options.Directory+".launch")
	if second.PID == first.PID {
		t.Fatalf("fresh recovery reused child process %d", first.PID)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	assertReapedAndUnlocked(t, m, ".launch")
}

func TestCapabilityAndSecretFailures(t *testing.T) {
	modes := []string{"restricted", "secret-error"}
	for _, capability := range []string{"agent/v1", "event/v1", "knowledge/v1", "memory/v1", "memory/query-v1", "space/catalog-v1", "space/organization-v1"} {
		modes = append(modes, "missing:"+capability)
	}
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			m := helperManager(t, mode, 3*time.Second)
			called := false
			err := m.WithClient(context.Background(), func(*client.Client) error { called = true; return nil })
			if err == nil || called {
				t.Fatalf("invalid hello accepted: called=%v err=%v", called, err)
			}
			record := readLaunch(t, m.options.Directory+".launch")
			if strings.Contains(err.Error(), record.Token) || strings.Contains(err.Error(), "private stderr") {
				t.Fatal("child secret leaked in error")
			}
			assertReapedAndUnlocked(t, m, ".launch")
		})
	}
}

func TestAmbiguousCommandIsNotRetried(t *testing.T) {
	m := helperManager(t, "disconnect", 3*time.Second)
	key := model.RequestKey{Principal: "gsea", Space: "test", RequestID: "stable-request"}
	calls := 0
	err := m.WithClient(context.Background(), func(c *client.Client) error {
		calls++
		_, err := c.Mutate(context.Background(), api.MutateRequest{Key: key})
		return err
	})
	var unknown *client.OutcomeUnknownError
	if !errors.As(err, &unknown) || unknown.Key != key || calls != 1 {
		t.Fatalf("ambiguous command lost or retried: calls=%d err=%v", calls, err)
	}
	assertReapedAndUnlocked(t, m, ".launch")
}

func TestTimeoutAndForcedCleanup(t *testing.T) {
	for _, mode := range []string{"init-failure", "init-hang", "hello-hang", "stubborn"} {
		t.Run(mode, func(t *testing.T) {
			m := helperManager(t, mode, 300*time.Millisecond)
			start := time.Now()
			err := m.WithClient(context.Background(), func(*client.Client) error { return nil })
			if err == nil || time.Since(start) > 3*time.Second {
				t.Fatalf("failure not bounded: elapsed=%v err=%v", time.Since(start), err)
			}
			suffix := ".launch"
			if strings.HasPrefix(mode, "init-") {
				suffix = ".init"
			}
			assertReapedAndUnlocked(t, m, suffix)
			if _, err := os.Stat(m.options.Directory + ".early-release"); !os.IsNotExist(err) {
				t.Fatal("ownership released before child exit")
			}
		})
	}
}

func TestLocalQueueCancellationAndClose(t *testing.T) {
	m := helperManager(t, "normal", 3*time.Second)
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	first := make(chan error, 1)
	go func() {
		first <- m.WithClient(context.Background(), func(*client.Client) error {
			close(entered)
			<-release
			return nil
		})
	}()
	select {
	case <-entered:
	case err := <-first:
		t.Fatalf("batch failed before callback: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	var calls atomic.Int32
	if err := m.WithClient(ctx, func(*client.Client) error { calls.Add(1); return nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("local queue cancellation=%v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("canceled queued callback executed")
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("active close cancellation=%v", err)
	}
	assertReapedAndUnlocked(t, m, ".launch")
	if err := m.WithClient(context.Background(), func(*client.Client) error { return nil }); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed manager accepted work: %v", err)
	}
}

func TestCrossProcessContentionAndRetry(t *testing.T) {
	m := helperManager(t, "normal", 3*time.Second)
	cmd := exec.Command(m.options.Binary, "hold-lock", m.options.Directory)
	cmd.Env = os.Environ()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { stdin.Close(); cmd.Process.Kill(); cmd.Wait() }()
	if _, err := io.ReadFull(stdout, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := m.WithClient(ctx, func(*client.Client) error { return errors.New("must not run") }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ownership timeout=%v", err)
	}
	if _, err := os.Stat(m.options.Directory); !os.IsNotExist(err) {
		t.Fatal("contender created store before acquiring ownership")
	}
	result := make(chan error, 1)
	go func() {
		result <- m.WithClient(context.Background(), func(*client.Client) error { return nil })
	}()
	stdin.Close()
	if err := <-result; err != nil {
		t.Fatalf("retry after ownership release: %v", err)
	}
	assertReapedAndUnlocked(t, m, ".launch")
}

func TestInvalidOptionsAndStartupFailure(t *testing.T) {
	for _, options := range []Options{
		{}, {Directory: "/"}, {Directory: "\x00"}, {Directory: "/tmp/ate", Timeout: -1},
		{Directory: "/tmp/ate", Timeout: 11 * time.Minute}, {Directory: "/tmp/ate", Binary: strings.Repeat("a", 4097)},
	} {
		if _, err := New(options); err == nil {
			t.Fatal("invalid options accepted")
		}
	}
	m, err := New(Options{Binary: filepath.Join(t.TempDir(), "missing"), Directory: filepath.Join(t.TempDir(), "store")})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.WithClient(context.Background(), func(*client.Client) error { return nil }); err == nil {
		t.Fatal("missing executable accepted")
	}
	lock, err := runtimeowner.Acquire(m.options.Directory)
	if err != nil {
		t.Fatalf("startup failure retained lock: %v", err)
	}
	lock.Close()
}

func TestTimeoutRetiresClientAndKeepsLocalCallbacksSerialized(t *testing.T) {
	m := helperManager(t, "normal", 300*time.Millisecond)
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	result := make(chan error, 1)
	go func() {
		result <- m.WithClient(context.Background(), func(c *client.Client) error {
			close(entered)
			// Simulate a callback that fails to return when its client closes.
			<-release
			_, err := c.GetState(context.Background(), api.ReadStateRequest{Space: "test"})
			return err
		})
	}()
	select {
	case <-entered:
	case err := <-result:
		t.Fatalf("batch failed before callback: %v", err)
	}
	if err := <-result; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("batch timeout=%v", err)
	}
	assertReapedAndUnlocked(t, m, ".launch")
	calls := 0
	err := m.WithClient(context.Background(), func(*client.Client) error { calls++; return nil })
	if !errors.Is(err, context.DeadlineExceeded) || calls != 0 {
		t.Fatalf("overlapping callbacks: calls=%d err=%v", calls, err)
	}
}

func TestPanicStillReapsAndReleasesOwnership(t *testing.T) {
	m := helperManager(t, "normal", 3*time.Second)
	func() {
		defer func() {
			if recover() != "callback panic" {
				t.Error("callback panic was not propagated")
			}
		}()
		_ = m.WithClient(context.Background(), func(*client.Client) error { panic("callback panic") })
	}()
	assertReapedAndUnlocked(t, m, ".launch")
	if err := m.WithClient(context.Background(), func(*client.Client) error { return nil }); err != nil {
		t.Fatalf("manager unusable after recovered panic: %v", err)
	}
}

func TestSymlinkAliasSharesOwnership(t *testing.T) {
	m := helperManager(t, "normal", 100*time.Millisecond)
	target := filepath.Join(t.TempDir(), "actual")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, m.options.Directory); err != nil {
		t.Fatal(err)
	}
	owner, err := runtimeowner.Acquire(target)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	called := false
	err = m.WithClient(context.Background(), func(*client.Client) error { called = true; return nil })
	if !errors.Is(err, context.DeadlineExceeded) || called {
		t.Fatalf("alias bypassed ownership: called=%v err=%v", called, err)
	}
}

func TestExistingContentsAreNeverReinitialized(t *testing.T) {
	m := helperManager(t, "normal", 3*time.Second)
	if err := os.Mkdir(m.options.Directory, 0700); err != nil {
		t.Fatal(err)
	}
	evidence := filepath.Join(m.options.Directory, "partial-store")
	if err := os.WriteFile(evidence, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.WithClient(context.Background(), func(*client.Client) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.options.Directory + ".init"); !os.IsNotExist(err) {
		t.Fatal("existing directory was reinitialized")
	}
	if data, err := os.ReadFile(evidence); err != nil || string(data) != "keep" {
		t.Fatal("existing contents changed")
	}
}

func TestRealATEPersistenceAcrossBatches(t *testing.T) {
	_, source, _, _ := runtime.Caller(0)
	sibling := filepath.Clean(filepath.Join(filepath.Dir(source), "../../../agent-temporal-engine"))
	if _, err := os.Stat(filepath.Join(sibling, "cmd", "ate", "main.go")); os.IsNotExist(err) {
		t.Skip("sibling ATE source is unavailable")
	}
	binary := filepath.Join(t.TempDir(), "ate")
	build := exec.Command("go", "build", "-o", binary, "./cmd/ate")
	build.Dir = sibling
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build current sibling ATE: %v\n%s", err, output)
	}
	directory := filepath.Join(t.TempDir(), "home", "temporal")
	m, err := New(Options{Binary: binary, Directory: directory})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ctx := context.Background()
	key := model.RequestKey{Space: "gsea-test", RequestID: "stable-write"}
	var initial api.StateSnapshot
	err = m.WithClient(ctx, func(c *client.Client) error {
		// The trusted host binds its bridge to the durable store before writing.
		// AllSpaces alone does not authorize this global snapshot request.
		snapshot, err := c.Snapshot(ctx)
		if err != nil {
			return fmt.Errorf("snapshot for initial store binding: %w", err)
		}
		initial = snapshot
		result, err := c.Mutate(ctx, api.MutateRequest{Key: key, Batch: api.StateBatch{Operations: []api.StateMutation{
			{Kind: api.MutationPut, Key: []byte("key"), Value: api.Value{Kind: api.ValueBytes, Data: []byte("durable")}},
		}}})
		if err != nil {
			return err
		}
		if result.Code != api.CodeDurableAck {
			return fmt.Errorf("mutation result: %s", result.Code)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// A new session also exercises the deployment default: ate is resolved
	// through PATH, while the first session used an explicit executable path.
	t.Setenv("PATH", filepath.Dir(binary)+string(os.PathListSeparator)+os.Getenv("PATH"))
	second, err := New(Options{Directory: directory})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if second.options.Binary != "ate" {
		t.Fatalf("default binary = %q, want ate", second.options.Binary)
	}
	err = second.WithClient(ctx, func(c *client.Client) error {
		snapshot, err := c.Snapshot(ctx)
		if err != nil {
			return fmt.Errorf("snapshot for reopened store binding: %w", err)
		}
		if snapshot.Fence.Store != initial.Fence.Store || snapshot.Fence.LSN <= initial.Fence.LSN {
			return errors.New("snapshot did not preserve store identity and committed progress")
		}
		result, err := c.GetState(ctx, api.ReadStateRequest{Space: key.Space, Key: []byte("key")})
		if err != nil {
			return err
		}
		if !result.Found || string(result.Value.Data) != "durable" {
			return errors.New("durable state missing after subprocess restart")
		}
		receipt, err := c.LookupRequest(ctx, api.LookupRequestRequest{Space: key.Space, RequestID: key.RequestID})
		if err == nil && receipt.Code != api.CodeDurableAck {
			return errors.New("original request receipt missing")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
