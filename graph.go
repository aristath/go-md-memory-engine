package memory

import (
	"context"
	"errors"
	"sort"
)

func allLinks(r Record) []Link {
	out := []Link{}
	seen := map[Link]bool{}
	add := func(l Link) {
		if !seen[l] {
			out = append(out, l)
			seen[l] = true
		}
	}
	for _, l := range r.Links {
		add(l)
	}
	for name, values := range r.Taxonomies {
		for _, v := range leaves(values) {
			if v.Type == "reference" {
				add(Link{"taxonomy:" + name, v.Ref})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Relation != out[j].Relation {
			return out[i].Relation < out[j].Relation
		}
		return out[i].Target < out[j].Target
	})
	return out
}

type Edge struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Relation string `json:"relation"`
	Resolved bool   `json:"resolved"`
}
type GraphOptions struct {
	Direction string `json:"direction,omitempty"`
	Relation  string `json:"relation,omitempty"`
	Depth     int    `json:"depth,omitempty"`
	Page
}
type GraphResult struct {
	Nodes       []string     `json:"nodes"`
	Edges       []Edge       `json:"edges"`
	Total       int          `json:"total"`
	NextOffset  *int         `json:"next_offset,omitempty"`
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
}

// Links returns paginated one-hop relationships, including unresolved targets.
func (s *Store) Links(ctx context.Context, id string, o GraphOptions) (GraphResult, error) {
	o.Depth = 1
	return s.Traverse(ctx, id, o)
}

// Traverse explores relationships by breadth-first search up to Depth hops.
func (s *Store) Traverse(ctx context.Context, id string, o GraphOptions) (GraphResult, error) {
	out := GraphResult{Nodes: []string{}, Edges: []Edge{}}
	e := s.with(ctx, false, func(tx *session) error {
		if _, ok := tx.state.Docs[id]; !ok {
			return ErrNotFound
		}
		if o.Depth < 0 {
			return errors.New("depth must be nonnegative")
		}
		if o.Depth == 0 {
			o.Depth = 1
		}
		if o.Direction == "" {
			o.Direction = "both"
		}
		if o.Direction != "both" && o.Direction != "outgoing" && o.Direction != "incoming" {
			return errors.New("direction must be both, outgoing or incoming")
		}
		adjacent := map[string][]Edge{}
		for from, d := range tx.state.Docs {
			for _, l := range allLinks(d.Record) {
				if o.Relation != "" && l.Relation != o.Relation {
					continue
				}
				_, resolved := tx.state.Docs[l.Target]
				edge := Edge{from, l.Target, l.Relation, resolved}
				if o.Direction != "incoming" {
					adjacent[from] = append(adjacent[from], edge)
				}
				if o.Direction != "outgoing" {
					adjacent[l.Target] = append(adjacent[l.Target], edge)
				}
			}
		}
		visited := map[string]bool{id: true}
		seenEdges := map[Edge]bool{}
		frontier := []string{id}
		for depth := 0; depth < o.Depth && len(frontier) > 0; depth++ {
			if e := ctx.Err(); e != nil {
				return e
			}
			next := []string{}
			for _, node := range frontier {
				for _, edge := range adjacent[node] {
					seenEdges[edge] = true
					neighbor := edge.To
					if neighbor == node {
						neighbor = edge.From
					}
					if !visited[neighbor] {
						visited[neighbor] = true
						next = append(next, neighbor)
					}
				}
			}
			frontier = next
		}
		for edge := range seenEdges {
			out.Edges = append(out.Edges, edge)
		}
		sort.Slice(out.Edges, func(i, j int) bool {
			a, b := out.Edges[i], out.Edges[j]
			if a.From != b.From {
				return a.From < b.From
			}
			if a.Relation != b.Relation {
				return a.Relation < b.Relation
			}
			return a.To < b.To
		})
		out.Total = len(out.Edges)
		start, end, e := o.Page.bounds(out.Total)
		if e != nil {
			return e
		}
		out.Edges = out.Edges[start:end]
		if end < out.Total {
			out.NextOffset = &end
		}
		nodes := map[string]bool{id: true}
		for _, edge := range out.Edges {
			nodes[edge.From] = true
			nodes[edge.To] = true
		}
		for node := range nodes {
			out.Nodes = append(out.Nodes, node)
		}
		sort.Strings(out.Nodes)
		out.Diagnostics = tx.state.Diagnostics
		return nil
	})
	return out, e
}
