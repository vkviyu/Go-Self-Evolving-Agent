// Package agentprofile defines immutable Agent configuration snapshots.
package agentprofile

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"
)

type ContextInheritance string

const (
	InheritNone    ContextInheritance = "none"
	InheritSummary ContextInheritance = "summary"
	InheritRecent  ContextInheritance = "recent"
	InheritFull    ContextInheritance = "full"
)

type WorkspaceMode string

const (
	WorkspaceSharedReadonly WorkspaceMode = "shared-readonly"
	WorkspaceSharedLease    WorkspaceMode = "shared-lease"
	WorkspaceGitWorktree    WorkspaceMode = "git-worktree"
	WorkspaceSnapshot       WorkspaceMode = "snapshot"
)

type Limits struct {
	MaxIterations      int
	MaxChildren        int
	MaxInheritedTokens int
	MaxInputTokens     int
	MaxOutputTokens    int
	MaxToolCalls       int
	MaxWallTime        time.Duration
}

type Profile struct {
	Name               string
	Description        string
	Revision           string
	ModelProfile       string
	Instruction        string
	Tools              []string
	DelegableTools     []string
	ContextInheritance ContextInheritance
	WorkspaceMode      WorkspaceMode
	Limits             Limits
	CanSpawn           bool
	CanManageTasks     bool
	CanSendMessages    bool
}

func (profile Profile) Validate() error {
	if profile.Name == "" || profile.Revision == "" {
		return errors.New("Agent profile requires name and revision")
	}
	if profile.ModelProfile == "" {
		return fmt.Errorf("Agent profile %q requires a model profile", profile.Name)
	}
	switch profile.ContextInheritance {
	case InheritNone, InheritSummary, InheritRecent, InheritFull:
	default:
		return fmt.Errorf("Agent profile %q has unsupported context inheritance %q", profile.Name, profile.ContextInheritance)
	}
	switch profile.WorkspaceMode {
	case WorkspaceSharedReadonly, WorkspaceSharedLease, WorkspaceGitWorktree, WorkspaceSnapshot:
	default:
		return fmt.Errorf("Agent profile %q has unsupported workspace mode %q", profile.Name, profile.WorkspaceMode)
	}
	if profile.Limits.MaxIterations <= 0 || profile.Limits.MaxToolCalls <= 0 {
		return fmt.Errorf("Agent profile %q requires positive iteration and tool-call limits", profile.Name)
	}
	if profile.Limits.MaxChildren < 0 || profile.Limits.MaxInheritedTokens < 0 || profile.Limits.MaxInputTokens < 0 || profile.Limits.MaxOutputTokens < 0 {
		return fmt.Errorf("Agent profile %q has negative token or child limit", profile.Name)
	}
	if profile.Limits.MaxWallTime <= 0 {
		return fmt.Errorf("Agent profile %q requires a positive wall-time limit", profile.Name)
	}
	return nil
}

func (profile Profile) clone() Profile {
	profile.Tools = slices.Clone(profile.Tools)
	profile.DelegableTools = slices.Clone(profile.DelegableTools)
	return profile
}

type Registry struct {
	mu         sync.RWMutex
	generation uint64
	profiles   map[string]Profile
	revisions  map[string]Profile
}

func NewRegistry() *Registry {
	return &Registry{profiles: make(map[string]Profile), revisions: make(map[string]Profile)}
}

func (registry *Registry) Activate(profiles ...Profile) (uint64, error) {
	if registry == nil {
		return 0, errors.New("Agent profile registry is nil")
	}
	if len(profiles) == 0 {
		return 0, errors.New("at least one Agent profile is required")
	}
	validated := make([]Profile, len(profiles))
	for index, profile := range profiles {
		if err := profile.Validate(); err != nil {
			return 0, err
		}
		validated[index] = profile.clone()
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	// Validate the entire publication before mutation, including conflicts
	// within a batch. Revisions cannot be rebound to different contents.
	batch := make(map[string]Profile, len(validated))
	for _, profile := range validated {
		previous, exists := batch[profile.Revision]
		if !exists {
			previous, exists = registry.revisions[profile.Revision]
		}
		if exists {
			oldID, _ := previous.Fingerprint()
			newID, _ := profile.Fingerprint()
			if oldID != newID {
				return 0, fmt.Errorf("Agent profile revision %q already identifies different content", profile.Revision)
			}
		}
		batch[profile.Revision] = profile
	}
	for _, profile := range validated {
		registry.profiles[profile.Name] = profile
		registry.revisions[profile.Revision] = profile
	}
	registry.generation++
	return registry.generation, nil
}

func (registry *Registry) Snapshot() *Snapshot {
	if registry == nil {
		return nil
	}
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	profiles := make(map[string]Profile, len(registry.profiles))
	for name, profile := range registry.profiles {
		profiles[name] = profile.clone()
	}
	revisions := make(map[string]Profile, len(registry.revisions))
	for revision, profile := range registry.revisions {
		revisions[revision] = profile.clone()
	}
	return &Snapshot{generation: registry.generation, profiles: profiles, revisions: revisions}
}

type Snapshot struct {
	generation uint64
	profiles   map[string]Profile
	revisions  map[string]Profile
}

func (snapshot *Snapshot) Generation() uint64 {
	if snapshot == nil {
		return 0
	}
	return snapshot.generation
}

func (snapshot *Snapshot) Get(name string) (Profile, bool) {
	if snapshot == nil {
		return Profile{}, false
	}
	profile, ok := snapshot.profiles[name]
	if !ok {
		return Profile{}, false
	}
	return profile.clone(), true
}

// GetRevision resolves the immutable profile revision captured by an Agent.
// Names are operator-facing aliases; revisions are the durable identity used
// for authorization and replay.
func (snapshot *Snapshot) GetRevision(revision string) (Profile, bool) {
	if snapshot == nil || revision == "" {
		return Profile{}, false
	}
	profile, ok := snapshot.revisions[revision]
	return profile.clone(), ok
}

func (snapshot *Snapshot) Names() []string {
	if snapshot == nil {
		return nil
	}
	names := make([]string, 0, len(snapshot.profiles))
	for name := range snapshot.profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
