package agentprofile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

var ErrSnapshot = errors.New("invalid or missing durable Agent profile snapshot")

type durableSnapshot struct {
	Version     int     `json:"version"`
	Fingerprint string  `json:"fingerprint"`
	Profile     Profile `json:"profile"`
}

func (profile Profile) Fingerprint() (string, error) {
	if err := profile.Validate(); err != nil {
		return "", err
	}
	profile = profile.clone()
	slices.Sort(profile.Tools)
	slices.Sort(profile.DelegableTools)
	encoded, err := json.Marshal(profile)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return "profile-sha256:" + hex.EncodeToString(digest[:]), nil
}

func Encode(profile Profile) (json.RawMessage, error) {
	fingerprint, err := profile.Fingerprint()
	if err != nil {
		return nil, err
	}
	return json.Marshal(durableSnapshot{Version: 1, Fingerprint: fingerprint, Profile: profile})
}

func Decode(data json.RawMessage, revision string) (Profile, error) {
	var snapshot durableSnapshot
	if len(data) == 0 {
		return Profile{}, ErrSnapshot
	}
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return Profile{}, fmt.Errorf("%w: %v", ErrSnapshot, err)
	}
	fingerprint, err := snapshot.Profile.Fingerprint()
	if err != nil || snapshot.Version != 1 || snapshot.Fingerprint != fingerprint || snapshot.Profile.Revision != revision {
		return Profile{}, fmt.Errorf("%w: profile identity does not match", ErrSnapshot)
	}
	return snapshot.Profile.clone(), nil
}

// A nil allowlist delegates to the host's policy; an explicit empty list
// permits no tools. Neither case can override policy or an ancestor restriction.
func (profile Profile) AllowsTool(name string) bool {
	if profile.Tools != nil && !slices.Contains(profile.Tools, name) {
		return false
	}
	if !profile.CanSpawn && (name == "agent_spawn" || name == "agent_continue" || name == "agent_resume") {
		return false
	}
	if !profile.CanManageTasks && strings.HasPrefix(name, "task_") && name != "task_list" {
		return false
	}
	if !profile.CanSendMessages && name == "agent_message" {
		return false
	}
	return true
}

// ConstrainDelegation intersects a child's grant with the parent's declared
// delegable surface. This is persisted, so grandchildren cannot widen it again.
func ConstrainDelegation(parent, child Profile) (Profile, error) {
	child = child.clone()
	delegable := intersectGrants(parent.Tools, parent.DelegableTools)
	child.Tools = intersectGrants(child.Tools, delegable)
	// A child may only redelegate its effective grant, not tools removed by
	// either its own allowlist or an ancestor's restriction.
	child.DelegableTools = intersectGrants(child.Tools, child.DelegableTools)
	child.CanSpawn = child.CanSpawn && parent.CanSpawn
	child.CanManageTasks = child.CanManageTasks && parent.CanManageTasks
	child.CanSendMessages = child.CanSendMessages && parent.CanSendMessages
	child.Limits.MaxIterations = min(child.Limits.MaxIterations, parent.Limits.MaxIterations)
	child.Limits.MaxToolCalls = min(child.Limits.MaxToolCalls, parent.Limits.MaxToolCalls)
	child.Limits.MaxWallTime = min(child.Limits.MaxWallTime, parent.Limits.MaxWallTime)
	child.Limits.MaxChildren = min(child.Limits.MaxChildren, parent.Limits.MaxChildren)
	child.Limits.MaxInheritedTokens = narrowOptionalLimit(child.Limits.MaxInheritedTokens, parent.Limits.MaxInheritedTokens)
	child.Limits.MaxInputTokens = narrowOptionalLimit(child.Limits.MaxInputTokens, parent.Limits.MaxInputTokens)
	child.Limits.MaxOutputTokens = narrowOptionalLimit(child.Limits.MaxOutputTokens, parent.Limits.MaxOutputTokens)
	if parent.WorkspaceMode == WorkspaceSharedReadonly && child.WorkspaceMode != WorkspaceSharedReadonly {
		return Profile{}, errors.New("read-only Agent cannot delegate a writable workspace")
	}
	return child, nil
}

// Zero token limits inherit the host policy rather than granting zero tokens.
// A bounded ancestor must therefore replace an unbounded child limit.
func narrowOptionalLimit(child, parent int) int {
	if parent == 0 {
		return child
	}
	if child == 0 {
		return parent
	}
	return min(child, parent)
}

// intersectGrants intersects two tool allowlists. A nil list imposes no limit,
// so the other list is returned; the result keeps the order of left.
func intersectGrants(left, right []string) []string {
	if left == nil {
		return slices.Clone(right)
	}
	if right == nil {
		return slices.Clone(left)
	}
	result := make([]string, 0)
	for _, item := range left {
		if slices.Contains(right, item) {
			result = append(result, item)
		}
	}
	return result
}
