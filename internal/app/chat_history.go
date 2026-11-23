package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateports"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	sessionate "github.com/vkviyu/go-self-evolving-agent/internal/session/atestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

// DiscoverChats reads native ATE history and orchestration metadata without
// opening an agent runtime or inspecting any per-session runtime database.
// Unregistered native conversations remain discoverable after catalog removal.
func DiscoverChats(ctx context.Context, cfg config.Config, id session.ID) (histories []session.ChatHistory, err error) {
	resources, closeFn, err := openCatalogAuthority(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, closeFn()) }()
	metadata, err := readChatMetadata(ctx, resources.records)
	if err != nil {
		return nil, err
	}
	candidates, err := nativeChatCandidates(ctx, resources, id)
	if err != nil {
		return nil, err
	}
	histories = make([]session.ChatHistory, 0, len(candidates))
	for _, candidate := range candidates {
		if strings.HasPrefix(string(candidate.id), "agent-thread_") || metadata.children[candidate.id] {
			continue
		}
		history, found, err := nativeChatHistory(ctx, resources.sessions, candidate)
		if err != nil {
			return nil, fmt.Errorf("read conversation %q: %w", candidate.id, err)
		}
		if found {
			history.Workspace = metadata.workspaces[candidate.id]
			histories = append(histories, history)
		}
	}
	slices.SortFunc(histories, func(a, b session.ChatHistory) int {
		if order := b.UpdatedAt.Compare(a.UpdatedAt); order != 0 {
			return order
		}
		return strings.Compare(string(a.ID), string(b.ID))
	})
	return histories, nil
}

type chatMetadata struct {
	children   map[session.ID]bool
	workspaces map[session.ID]string
}

// Read all relevant orchestration records at one authority anchor. Only Tx
// operations run in this callback; native session reads acquire their own
// process leases after it ends.
func readChatMetadata(ctx context.Context, records *ateauthority.Store) (chatMetadata, error) {
	result := make(chan chatMetadata, 1)
	err := records.View(ctx, func(tx *ateauthority.Tx) error {
		metadata := chatMetadata{children: make(map[session.ID]bool), workspaces: make(map[session.ID]string)}
		roots := make(map[session.ID]orchestration.TeamID)
		if err := tx.List("gsea/v1/root/", func(_ string, raw json.RawMessage) error {
			var root orchestration.RootThread
			if err := json.Unmarshal(raw, &root); err != nil {
				return err
			}
			roots[root.SessionID] = root.TeamID
			return nil
		}); err != nil {
			return err
		}
		if err := tx.List("gsea/v1/agent/", func(_ string, raw json.RawMessage) error {
			var agent orchestration.AgentSnapshot
			if err := json.Unmarshal(raw, &agent); err != nil {
				return err
			}
			if agent.ParentAgentID != "" {
				metadata.children[agent.SessionID] = true
			}
			return nil
		}); err != nil {
			return err
		}
		workspaces := make(map[orchestration.TeamID]string)
		ambiguous := make(map[orchestration.TeamID]bool)
		if err := tx.List("gsea/v1/workspace-view/", func(_ string, raw json.RawMessage) error {
			var view workspacecoord.View
			if err := json.Unmarshal(raw, &view); err != nil {
				return err
			}
			if view.WorkspaceRoot != "" {
				if previous := workspaces[view.TeamID]; previous != "" && previous != view.WorkspaceRoot {
					ambiguous[view.TeamID] = true
				}
				workspaces[view.TeamID] = view.WorkspaceRoot
			}
			return nil
		}); err != nil {
			return err
		}
		for id, team := range roots {
			if !ambiguous[team] {
				metadata.workspaces[id] = workspaces[team]
			}
		}
		result <- metadata
		return nil
	})
	if err != nil {
		return chatMetadata{}, err
	}
	select {
	case metadata := <-result:
		return metadata, nil
	default:
		return chatMetadata{}, errors.New("conversation metadata read did not complete")
	}
}

type chatCandidate struct {
	id       session.ID
	sequence uint64
}

func nativeChatCandidates(ctx context.Context, resources *authorityResources, id session.ID) ([]chatCandidate, error) {
	if id != "" {
		sequence, err := resources.sessions.CurrentSequence(ctx, id)
		if err != nil {
			return nil, err
		}
		if sequence == 0 {
			return nil, nil
		}
		return []chatCandidate{{id: id, sequence: sequence}}, nil
	}
	result := make(chan []chatCandidate, 1)
	err := resources.process.WithClient(ctx, func(c *client.Client) error {
		request := api.ListSessionsRequest{Space: authoritySpace, Principal: "gsea", Limit: api.MaxSessionPageItems}
		items := make([]chatCandidate, 0)
		last := ""
		total := -1
		for {
			page, err := c.ListSessions(ctx, request)
			if err != nil {
				return err
			}
			if page.Anchor.Space != authoritySpace || page.Anchor.Fence.Store != resources.storeID ||
				page.Anchor.Validate() != nil || (request.Anchor != nil && page.Anchor != *request.Anchor) {
				return sessionate.ErrIdentity
			}
			if page.Total > ateauthority.MaxListEntries {
				return sessionate.ErrLimit
			}
			if total < 0 {
				total = page.Total
			}
			if page.Total != total || len(page.Items) > request.Limit {
				return sessionate.ErrCorrupt
			}
			for _, item := range page.Items {
				if item.ID <= last || item.Space != authoritySpace || item.Owner != "gsea" {
					return sessionate.ErrCorrupt
				}
				items = append(items, chatCandidate{id: session.ID(item.ID), sequence: item.Sequence})
				last = item.ID
			}
			if page.Continuation == "" {
				if len(items) != total {
					return sessionate.ErrCorrupt
				}
				result <- items
				return nil
			}
			if len(page.Items) == 0 || len(items) >= total || page.Continuation == request.Continuation {
				return sessionate.ErrCorrupt
			}
			anchor := page.Anchor
			request.Anchor, request.Continuation = &anchor, page.Continuation
		}
	})
	if err != nil {
		return nil, err
	}
	select {
	case candidates := <-result:
		return candidates, nil
	default:
		return nil, errors.New("native session listing did not complete")
	}
}

// HistoryThrough preserves the immutable sequence cut obtained during native
// discovery even when another process appends while we inspect the first prompt.
func nativeChatHistory(ctx context.Context, events *sessionate.Store, candidate chatCandidate) (session.ChatHistory, bool, error) {
	history := session.ChatHistory{ID: candidate.id}
	if candidate.sequence == 0 {
		return history, false, nil
	}
	last, err := events.HistoryThrough(ctx, string(candidate.id), candidate.sequence-1, candidate.sequence, 1)
	if err != nil {
		return history, false, err
	}
	if len(last) != 1 {
		return history, false, sessionate.ErrCorrupt
	}
	history.UpdatedAt = last[0].OccurredAt
	var after uint64
	found := false
	for after < candidate.sequence {
		if after >= ateports.MaxLimit {
			return history, false, sessionate.ErrLimit
		}
		page, err := events.HistoryThrough(ctx, string(candidate.id), after, candidate.sequence, api.MaxSessionPageItems)
		if err != nil {
			return history, false, err
		}
		if len(page) == 0 {
			return history, false, sessionate.ErrCorrupt
		}
		for _, record := range page {
			if history.CreatedAt.IsZero() {
				history.CreatedAt = record.OccurredAt
			}
			after = record.Sequence
			if record.Kind == string(session.EventTurnStarted) {
				found = true
			}
			if record.Kind == string(session.EventUserMessage) {
				var event session.Event
				if err := json.Unmarshal(record.Payload, &event); err != nil {
					return history, false, err
				}
				var message struct {
					Content string `json:"content"`
				}
				if err := json.Unmarshal(event.Payload, &message); err != nil {
					return history, false, err
				}
				prompt := []rune(message.Content)
				history.Prompt = string(prompt[:min(len(prompt), 240)])
				return history, true, nil
			}
		}
	}
	return history, found, nil
}
