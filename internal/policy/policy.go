// Package policy owns authorization decisions. Executors consume decisions but
// never interpret user configuration directly.
package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

// Effect is the outcome of a policy evaluation.
type Effect string

const (
	EffectAllow           Effect = "allow"
	EffectDeny            Effect = "deny"
	EffectRequireApproval Effect = "require_approval"
)

var ErrApprovalUnavailable = errors.New("approval is required but no approver is available")

// Request contains the stable identity and normalized resource being authorized.
type Request struct {
	Actor         orchestration.ActorScope
	CallID        string
	SessionID     string
	ToolName      string
	Revision      string
	Capability    string
	Resource      string
	RiskClass     string
	NormalizedArg json.RawMessage
}

// Decision is an auditable policy result.
type Decision struct {
	Effect Effect
	RuleID string
	Reason string
}

// Engine evaluates an authorization request without performing the action.
type Engine interface {
	Evaluate(ctx context.Context, request Request) (Decision, error)
}

// Approver resolves a require-approval decision. Implementations may interrupt
// an Agent runner rather than blocking a terminal directly.
type Approver interface {
	Approve(ctx context.Context, request Request, decision Decision) (bool, error)
}

// Rule is a first-match policy rule. Empty patterns mean "*". ResourcePrefix
// is matched after path/URL/command normalization by the caller.
type Rule struct {
	ID                string
	ToolPattern       string
	CapabilityPattern string
	ResourcePrefix    string
	Effect            Effect
	Reason            string
}

// StaticEngine is a deterministic first-match rule engine with an explicit default.
type StaticEngine struct {
	rules         []Rule
	defaultEffect Effect
}

// NewStaticEngine validates and copies a ruleset.
func NewStaticEngine(defaultEffect Effect, rules ...Rule) (*StaticEngine, error) {
	if err := validateEffect(defaultEffect); err != nil {
		return nil, fmt.Errorf("default effect: %w", err)
	}
	copyRules := append([]Rule(nil), rules...)
	for index := range copyRules {
		rule := &copyRules[index]
		if rule.ID == "" {
			return nil, fmt.Errorf("policy rule %d: ID is required", index)
		}
		if rule.ToolPattern == "" {
			rule.ToolPattern = "*"
		}
		if rule.CapabilityPattern == "" {
			rule.CapabilityPattern = "*"
		}
		if _, err := path.Match(rule.ToolPattern, "probe"); err != nil {
			return nil, fmt.Errorf("policy rule %q tool pattern: %w", rule.ID, err)
		}
		if _, err := path.Match(rule.CapabilityPattern, "probe"); err != nil {
			return nil, fmt.Errorf("policy rule %q capability pattern: %w", rule.ID, err)
		}
		if err := validateEffect(rule.Effect); err != nil {
			return nil, fmt.Errorf("policy rule %q: %w", rule.ID, err)
		}
	}
	return &StaticEngine{rules: copyRules, defaultEffect: defaultEffect}, nil
}

// DefaultDeny returns an engine that denies every request unless a rule allows it.
func DefaultDeny(rules ...Rule) (*StaticEngine, error) {
	return NewStaticEngine(EffectDeny, rules...)
}

// Evaluate returns the first matching decision and never mutates the request.
func (e *StaticEngine) Evaluate(_ context.Context, request Request) (Decision, error) {
	if request.ToolName == "" || request.Capability == "" {
		return Decision{}, errors.New("policy request requires tool name and capability")
	}
	for _, rule := range e.rules {
		toolMatch, _ := path.Match(rule.ToolPattern, request.ToolName)
		capabilityMatch, _ := path.Match(rule.CapabilityPattern, request.Capability)
		resourceMatch := resourceWithin(request.Resource, rule.ResourcePrefix)
		if toolMatch && capabilityMatch && resourceMatch {
			return Decision{Effect: rule.Effect, RuleID: rule.ID, Reason: rule.Reason}, nil
		}
	}
	return Decision{
		Effect: e.defaultEffect,
		RuleID: "default",
		Reason: "no policy rule matched",
	}, nil
}

func resourceWithin(resource, prefix string) bool {
	if prefix == "" || resource == prefix {
		return true
	}
	if strings.HasSuffix(prefix, "/") || strings.HasSuffix(prefix, ":") {
		return strings.HasPrefix(resource, prefix)
	}
	return strings.HasPrefix(resource, prefix+"/")
}

func validateEffect(effect Effect) error {
	switch effect {
	case EffectAllow, EffectDeny, EffectRequireApproval:
		return nil
	default:
		return fmt.Errorf("unknown policy effect %q", effect)
	}
}
