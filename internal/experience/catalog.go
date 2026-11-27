package experience

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/agent-temporal-engine/memory"
	"github.com/vkviyu/agent-temporal-engine/model"
	"github.com/vkviyu/agent-temporal-engine/spacecatalog"
)

type (
	directory struct{ Spaces []spacecatalog.Space }
	catalog   struct {
		Spaces    []spacecatalog.Space
		Entries   map[model.SpaceID][]spacecatalog.Entry
		Relations map[model.SpaceID][]spacecatalog.Relation
		Memories  []memoryItem
	}
	memoryItem struct {
		ID    string
		Score uint32
		// ClaimText exposes UTF-8 to the model; native Memory.Claim encodes as
		// base64 in JSON and remains intact for exact native record handling.
		ClaimText string
		Source    memory.Source
		Memory    memory.Memory
	}
)

func operational(id model.SpaceID) bool {
	// Native session/state authority and execution partitions are infrastructure,
	// not semantic categories offered to the curator.
	return id == "gsea-authority" || strings.HasPrefix(string(id), "gsea-history-") || strings.HasPrefix(string(id), "gsea-execution-")
}

// budget checks both catalog item count and serialized size, returning ErrIncomplete if
// either limit is exceeded.
func budget(v any, count int) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if count > maxCatalogItems || len(raw) > maxCatalogBytes {
		return ErrIncomplete
	}
	return nil
}

func pageEnd(truncated bool, next, previous string) (bool, error) {
	if !truncated && next == "" {
		return true, nil
	}
	if !truncated || next == "" || next == previous {
		return false, ErrIncomplete
	}
	return false, nil
}

func (s *Service) directory(ctx context.Context) (directory, error) {
	var out directory
	err := s.bridge.WithClient(ctx, func(c *client.Client) error {
		r := api.ListSpacesRequest{Limit: maxPageItems}
		for pages := 0; pages < 64; pages++ {
			p, err := c.ListSpaces(ctx, r)
			if err != nil {
				return err
			}
			for _, space := range p.Items {
				// Only registered catalog spaces are semantic categories. Native
				// authority/decision partitions share the store but are not
				// something the curator may rename or archive.
				if space.Registered && space.ID != s.bridge.OperationalSpace() && !operational(space.ID) {
					out.Spaces = append(out.Spaces, space)
				}
			}
			if err = budget(out, len(out.Spaces)); err != nil {
				return err
			}
			done, err := pageEnd(p.Truncated, p.Continuation, r.Continuation)
			if err != nil {
				return err
			}
			if done {
				return nil
			}
			r.Continuation = p.Continuation
		}
		return ErrIncomplete
	})
	// A process deadline may return before its callback finishes unwinding.
	// Do not read callback-owned result memory on the error path.
	if err != nil {
		return directory{}, err
	}
	return out, nil
}

const routeSchema = `{"space_ids":[],"new_space":null,"search_terms":[],"full_history":false}`

func (s *Service) route(ctx context.Context, key, goal string, evidence any, generate Generate) (route, directory, error) {
	var d directory
	if found, err := s.load(ctx, key+"/directory", &d); err != nil {
		return route{}, d, err
	} else if !found {
		d, err = s.directory(ctx)
		if err != nil {
			return route{}, d, err
		}
		if err = s.save(ctx, key+"/directory", d); err != nil {
			return route{}, d, err
		}
	}
	allowed := map[string]bool{}
	for _, v := range d.Spaces {
		allowed[string(v.ID)] = true
	}
	var r route
	err := s.decide(ctx, key+"/route", "discover", routeSchema,
		`All four fields are required. Choose semantically relevant existing spaces across ALL domains. space_ids must contain only supplied IDs, at most 32 including any proposed new space. Do not filter by project/session. Reuse suitable spaces; when needed replace new_space:null with {"name":"New name","description":"Scope and applicability"}, with a nonempty name at most 512 bytes and description at most 8192 bytes. An empty directory may yield a new space or no spaces. Identify requests to summarize/reflect on experience semantically, in any language, by full_history=true. Generate at most 8 search_terms, each at most 256 bytes, complementary to the complete literal goal.`,
		struct {
			Goal      string
			Directory directory
			Evidence  any
		}{goal, d, evidence}, generate, &r, func() error {
			if !stringsValid(r.SpaceIDs, MaxSpaces, 1024) || !stringsValid(r.SearchTerms, 8, 256) {
				return ErrDecision
			}
			for _, id := range r.SpaceIDs {
				if !allowed[id] {
					return ErrDecision
				}
			}
			if r.NewSpace != nil && (!boundedText(r.NewSpace.Name, 512, true) || !boundedText(r.NewSpace.Description, 8192, true) || len(r.SpaceIDs) >= MaxSpaces) {
				return ErrDecision
			}
			return nil
		})
	return r, d, err
}

func routeSpaces(key string, r route) []model.SpaceID {
	out := make([]model.SpaceID, 0, len(r.SpaceIDs)+1)
	for _, id := range r.SpaceIDs {
		out = append(out, model.SpaceID(id))
	}
	if r.NewSpace != nil {
		out = append(out, model.SpaceID("gsea-space-"+digest(key)))
	}
	return out
}

func (s *Service) applyRoute(ctx context.Context, key string, r route) error {
	if r.NewSpace == nil {
		return nil
	}
	body, err := spacecatalog.EncodeApply(spacecatalog.Apply{Name: r.NewSpace.Name, Description: r.NewSpace.Description})
	if err != nil {
		return err
	}
	_, err = s.commit(ctx, key+"/create-space", api.CommitRequest{Space: model.SpaceID("gsea-space-" + digest(key)), Method: api.MethodApplySpace, Body: body})
	return err
}

// queryAll fixes one publication cut per query and completes pagination or
// fails explicitly. Empty Text is used only for exact-ID current reuse gates.
func queryAll(ctx context.Context, c *client.Client, r api.QueryMemoryRequest) ([]memoryItem, error) {
	if len(r.Spaces) == 0 {
		return nil, nil
	}
	r.Limit = maxPageItems
	var out []memoryItem
	for pages := 0; pages < 64; pages++ {
		p, err := c.QueryMemory(ctx, r)
		if err != nil {
			return nil, err
		}
		for _, item := range p.Items {
			src := memory.Source{SourceID: item.Memory.ID, Anchor: model.SnapshotAnchor{Space: item.Memory.Space, Fence: p.Fence}}
			out = append(out, memoryItem{ID: memoryIdentity(src), ClaimText: string(item.Memory.Claim), Source: src, Memory: item.Memory, Score: item.Score})
		}
		if err = budget(out, len(out)); err != nil {
			return nil, err
		}
		done, err := pageEnd(p.Truncated, p.Continuation, r.Continuation)
		if err != nil {
			return nil, err
		}
		if done {
			return out, nil
		}
		r.Continuation = p.Continuation
	}
	return nil, ErrIncomplete
}

// memoryIdentity pairs the source's space and source ID for deduplication across memory
// results.
func memoryIdentity(src memory.Source) string {
	return string(src.Anchor.Space) + "/" + string(src.SourceID)
}

func (s *Service) retrieve(ctx context.Context, spaces []model.SpaceID, goal string, terms []string) (catalog, error) {
	out := catalog{Entries: map[model.SpaceID][]spacecatalog.Entry{}, Relations: map[model.SpaceID][]spacecatalog.Relation{}}
	err := s.bridge.WithClient(ctx, func(c *client.Client) error {
		byID := map[string]memoryItem{}
		for _, term := range append([]string{goal}, terms...) {
			items, err := queryAll(ctx, c, api.QueryMemoryRequest{Spaces: spaces, Text: term, AtTime: time.Now().UTC()})
			if err != nil {
				return err
			}
			for _, v := range items {
				if _, ok := byID[v.ID]; !ok {
					byID[v.ID] = v
				}
			}
		}
		refs := map[string]memory.Source{}
		for _, id := range spaces {
			space, err := c.GetSpace(ctx, api.GetSpaceRequest{Space: id})
			if err != nil {
				return err
			}
			if !space.Found {
				return ErrSelection
			}
			out.Spaces = append(out.Spaces, space.Space)
			entries, relations, err := organizationPages(ctx, c, id)
			if err != nil {
				return err
			}
			out.Entries[id], out.Relations[id] = entries, relations
			for _, v := range entries {
				refs[memoryIdentity(v.Source)] = v.Source
			}
			for _, v := range relations {
				refs[memoryIdentity(v.From)] = v.From
				refs[memoryIdentity(v.To)] = v.To
			}
		}
		// Organizational references may point outside the chosen domain. Fetch
		// current eligible originals in those origin spaces, then keep exact IDs.
		origins := map[model.SpaceID]bool{}
		for _, v := range refs {
			if v.Anchor.Space != s.bridge.OperationalSpace() && !operational(v.Anchor.Space) {
				origins[v.Anchor.Space] = true
			}
		}
		if len(origins) > MaxSpaces {
			return ErrIncomplete
		}
		originIDs := sortedSpaces(origins)
		current, err := queryAll(ctx, c, api.QueryMemoryRequest{Spaces: originIDs, AtTime: time.Now().UTC()})
		if err != nil {
			return err
		}
		for _, v := range current {
			if ref, ok := refs[v.ID]; ok {
				// Authenticate the exact historical organizational reference.
				if err := readMemory(ctx, c, ref); err != nil {
					return err
				}
				v.Source = ref
				byID[v.ID] = v
			}
		}
		for _, v := range byID {
			out.Memories = append(out.Memories, v)
		}
		sort.Slice(out.Memories, func(i, j int) bool { return out.Memories[i].ID < out.Memories[j].ID })
		return budget(out, len(out.Memories)+len(refs))
	})
	if err != nil {
		return catalog{}, err
	}
	return out, nil
}

func sortedSpaces(set map[model.SpaceID]bool) []model.SpaceID {
	out := make([]model.SpaceID, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func organizationPages(ctx context.Context, c *client.Client, id model.SpaceID) ([]spacecatalog.Entry, []spacecatalog.Relation, error) {
	var entries []spacecatalog.Entry
	var relations []spacecatalog.Relation
	er := api.ListSpaceMemoriesRequest{Space: id, Limit: maxPageItems}
	for pages := 0; ; pages++ {
		if pages >= 64 {
			return nil, nil, ErrIncomplete
		}
		p, err := c.ListSpaceMemories(ctx, er)
		if err != nil {
			return nil, nil, err
		}
		entries = append(entries, p.Items...)
		if err = budget(entries, len(entries)); err != nil {
			return nil, nil, err
		}
		done, err := pageEnd(p.Truncated, p.Continuation, er.Continuation)
		if err != nil {
			return nil, nil, err
		}
		if done {
			break
		}
		er.Continuation = p.Continuation
	}
	rr := api.ListMemoryRelationsRequest{Space: id, Limit: maxPageItems}
	for pages := 0; ; pages++ {
		if pages >= 64 {
			return nil, nil, ErrIncomplete
		}
		p, err := c.ListMemoryRelations(ctx, rr)
		if err != nil {
			return nil, nil, err
		}
		relations = append(relations, p.Items...)
		if err = budget(relations, len(relations)); err != nil {
			return nil, nil, err
		}
		done, err := pageEnd(p.Truncated, p.Continuation, rr.Continuation)
		if err != nil {
			return nil, nil, err
		}
		if done {
			break
		}
		rr.Continuation = p.Continuation
	}
	return entries, relations, nil
}

func readMemory(ctx context.Context, c *client.Client, src memory.Source) (err error) {
	view, err := c.OpenView(ctx, api.OpenViewRequest{Space: src.Anchor.Space, Anchor: &src.Anchor})
	if err != nil {
		return err
	}
	defer func() {
		closeErr := c.CloseView(ctx, api.CloseViewRequest{Ref: view.Ref})
		if err == nil {
			err = closeErr
		}
	}()
	_, err = c.GetNode(ctx, api.GetNodeRequest{View: view.Ref, Ref: model.NodeRef{
		Kind: model.NodeEntity, Entity: model.EntityKey{Profile: "memory", Kind: "memory", ID: model.EntityID(src.SourceID)},
	}})
	if err != nil {
		return fmt.Errorf("experience: exact memory read: %w", err)
	}
	return nil
}
