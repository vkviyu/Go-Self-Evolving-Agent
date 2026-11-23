// Package experience implements model-directed, evidence-grounded native ATE
// memory. The host calls Prepare automatically, Check immediately before every
// dispatch (including pending replay), and Complete after persisting the turn.
package experience

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/go-self-evolving-agent/internal/atebridge"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateports"
	"github.com/vkviyu/go-self-evolving-agent/internal/experienceport"
)

const (
	MaxSpaces        = 32
	MaxContextBytes  = 32 << 10
	MaxHistoryEvents = 8192
	MaxHistoryBytes  = 16 << 20
	MaxGoalBytes     = experienceport.MaxGoalBytes
	maxPageItems     = 128
	maxCatalogItems  = 4096
	maxCatalogBytes  = 2 << 20
	maxSelected      = 16
	maxModelBytes    = 64 << 10
)

var (
	ErrIncomplete = errors.New("experience: bounded evidence or catalog scan incomplete")
	ErrDecision   = errors.New("experience: invalid model decision")
	ErrSelection  = errors.New("experience: durable selection mismatch or no longer reusable")
)

type (
	HistoryReader = ateports.HistoryReader
	StateStore    = ateports.StateStore

	Options struct {
		Bridge *atebridge.Bridge
		// State stores semantic decisions in the same ATE authority.
		State StateStore
		// History must expose frozen cuts and original native evidence sources.
		History HistoryReader
	}
	Generate = experienceport.Generate
	Service  struct {
		bridge  *atebridge.Bridge
		state   StateStore
		history HistoryReader
		gate    chan struct{}
	}
	Context   = experienceport.Context
	Selection = experienceport.Selection
)

var serviceGates sync.Map

func New(o Options) (*Service, error) {
	if o.Bridge == nil || nilPort(o.State) || nilPort(o.History) {
		return nil, errors.New("experience: bridge, ATE state store, and explicit history reader required")
	}
	if _, ok := o.History.(EvidenceSourceProvider); !ok {
		return nil, errors.New("experience: native authority requires History with EvidenceSourceProvider")
	}
	if _, ok := o.History.(ateports.HistoryCutStore); !ok {
		return nil, errors.New("experience: native authority requires HistoryCut and HistoryThrough")
	}
	gate, _ := serviceGates.LoadOrStore(o.Bridge.OperationalSpace(), make(chan struct{}, 1))
	return &Service{bridge: o.Bridge, state: o.State, history: o.History, gate: gate.(chan struct{})}, nil
}

func nilPort(value any) bool {
	if value == nil {
		return true
	}
	switch reflect.ValueOf(value).Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflect.ValueOf(value).IsNil()
	default:
		return false
	}
}

func (s *Service) lock(ctx context.Context) (func(), error) {
	if ctx == nil {
		return nil, errors.New("experience: context required")
	}
	select {
	case s.gate <- struct{}{}:
		return func() { <-s.gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func digest(v any) string {
	raw, _ := json.Marshal(v)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (s *Service) key(session, turn string) string {
	return "experience/v1/" + digest([]string{string(s.bridge.OperationalSpace()), session, turn})
}

func (s *Service) load(ctx context.Context, key string, out any) (bool, error) {
	raw, found, err := s.state.Get(ctx, key)
	if err != nil || !found {
		return found, err
	}
	return true, json.Unmarshal(raw, out)
}

// save reads back the insert-once winner, including across Service instances.
func (s *Service) save(ctx context.Context, key string, value any) error {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	// HTML escaping would inflate bounded text such as a full goal past the
	// native state value limit. The bytes are still canonical JSON.
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return err
	}
	return s.state.Put(ctx, key, bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
}

func (s *Service) commit(ctx context.Context, key string, request api.CommitRequest) (api.CommitResult, error) {
	return s.bridge.ExecuteInSpace(ctx, request.Space, key, func(*client.Client) (api.CommitRequest, error) { return request, nil })
}

func validInput(session, turn, goal string) error {
	if strings.TrimSpace(session) == "" || strings.TrimSpace(turn) == "" || len(session) > 1024 || len(turn) > 1024 || len(goal) > MaxGoalBytes {
		return fmt.Errorf("%w: session/turn required; identity limit 1024 bytes and goal limit %d bytes", ErrDecision, MaxGoalBytes)
	}
	return nil
}
