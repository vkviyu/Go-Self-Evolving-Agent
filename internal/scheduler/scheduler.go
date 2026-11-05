// Package scheduler contains bounded, provider-aware admission primitives.
// It is deliberately independent of model execution and durable storage so
// the Supervisor can use the same rules in TUI and headless runtimes.
package scheduler

import (
	"maps"
	"sort"
	"sync"
)

// BucketSet limits concurrent runs per model/provider bucket. A zero or
// negative configured limit means unlimited for that bucket.
type BucketSet struct {
	mu           sync.Mutex
	limits       map[string]int
	active       map[string]int
	defaultLimit int
}

// NewBucketSet applies defaultLimit to every bucket without its own limit.
func NewBucketSet(defaultLimit int, limits map[string]int) *BucketSet {
	return &BucketSet{limits: maps.Clone(limits), active: make(map[string]int), defaultLimit: defaultLimit}
}

// TryAcquire takes a slot of bucket key without waiting. It reports false
// when the bucket is at its limit.
func (set *BucketSet) TryAcquire(key string) bool {
	if set == nil {
		return true
	}
	set.mu.Lock()
	defer set.mu.Unlock()
	limit, ok := set.limits[key]
	if !ok {
		limit = set.defaultLimit
	}
	if limit > 0 && set.active[key] >= limit {
		return false
	}
	set.active[key]++
	return true
}

// Release returns a slot taken by TryAcquire.
// The return value reports whether a matching active slot existed.
func (set *BucketSet) Release(key string) bool {
	if set == nil {
		return false
	}
	set.mu.Lock()
	defer set.mu.Unlock()
	if set.active[key] > 0 {
		set.active[key]--
		return true
	}
	return false
}

type Candidate struct {
	TeamKey  string
	ItemKey  string
	Priority int
}

// RoundRobin orders queued candidates by team. Each call advances a
// cursor, so repeatedly scanning the database cannot starve later Teams.
// Priority remains the primary ordering inside a Team.
type RoundRobin struct {
	mu     sync.Mutex
	cursor int
}

// WeightedRoundRobin is the historical name; no configurable Team weights
// exist in this scheduler. Kept as an alias for source compatibility.
// Deprecated: use RoundRobin.
type WeightedRoundRobin = RoundRobin

func (queue *RoundRobin) Order(items []Candidate) []Candidate {
	if len(items) < 2 {
		return append([]Candidate(nil), items...)
	}
	groups := make(map[string][]Candidate)
	teamOrder := make([]string, 0)
	for _, item := range items {
		if _, exists := groups[item.TeamKey]; !exists {
			teamOrder = append(teamOrder, item.TeamKey)
		}
		groups[item.TeamKey] = append(groups[item.TeamKey], item)
	}
	sort.Strings(teamOrder)
	for _, team := range teamOrder {
		sort.SliceStable(groups[team], func(i, j int) bool {
			if groups[team][i].Priority != groups[team][j].Priority {
				return groups[team][i].Priority > groups[team][j].Priority
			}
			return groups[team][i].ItemKey < groups[team][j].ItemKey
		})
	}
	queue.mu.Lock()
	start := queue.cursor % len(teamOrder)
	queue.cursor = (queue.cursor + 1) % len(teamOrder)
	queue.mu.Unlock()
	ordered := make([]Candidate, 0, len(items))
	for offset := 0; ; offset++ {
		added := false
		for index := 0; index < len(teamOrder); index++ {
			team := teamOrder[(start+index)%len(teamOrder)]
			group := groups[team]
			if offset < len(group) {
				ordered = append(ordered, group[offset])
				added = true
			}
		}
		if !added {
			break
		}
	}
	return ordered
}
