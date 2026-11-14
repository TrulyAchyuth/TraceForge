// Package tree reconstructs span trees from captured dataset spans and
// provides the structural analysis shared by the investigation and replay
// engines: exclusive time, callee resolution, error origin and retry groups.
package tree

import (
	"sort"
	"strings"

	"github.com/TrulyAchyuth/TraceForge/internal/model"
)

// Node is a span positioned in its trace tree.
type Node struct {
	Span     *model.DatasetSpan
	Parent   *Node
	Children []*Node // ordered by start time, then span ID
	StartNs  int64
	EndNs    int64
}

// Trace is a reconstructed trace.
type Trace struct {
	ID    string
	Phase string
	Root  *Node
	Nodes []*Node
	// Complete is true when the trace has exactly one root and every other
	// span's parent is present. Incomplete traces are not replayed.
	Complete bool
}

// Build groups spans by trace and links parents to children. Traces are
// returned ordered by trace ID so downstream processing is deterministic.
func Build(spans []model.DatasetSpan) []*Trace {
	byTrace := map[string][]*model.DatasetSpan{}
	for i := range spans {
		byTrace[spans[i].TraceID] = append(byTrace[spans[i].TraceID], &spans[i])
	}
	ids := make([]string, 0, len(byTrace))
	for id := range byTrace {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]*Trace, 0, len(ids))
	for _, id := range ids {
		group := byTrace[id]
		t := &Trace{ID: id, Phase: group[0].Phase, Complete: true}
		nodes := make(map[string]*Node, len(group))
		for _, sp := range group {
			n := &Node{Span: sp, StartNs: sp.Start.UnixNano()}
			n.EndNs = n.StartNs + sp.DurationNs
			nodes[sp.SpanID] = n
			t.Nodes = append(t.Nodes, n)
		}
		roots := 0
		for _, n := range t.Nodes {
			if n.Span.ParentSpanID == "" {
				roots++
				t.Root = n
				continue
			}
			parent, ok := nodes[n.Span.ParentSpanID]
			if !ok || parent == n {
				t.Complete = false
				continue
			}
			n.Parent = parent
			parent.Children = append(parent.Children, n)
		}
		if roots != 1 {
			t.Complete = false
		}
		if t.Complete && hasCycle(t.Root, len(t.Nodes)) {
			t.Complete = false
		}
		for _, n := range t.Nodes {
			sort.SliceStable(n.Children, func(i, j int) bool {
				a, b := n.Children[i], n.Children[j]
				if a.StartNs != b.StartNs {
					return a.StartNs < b.StartNs
				}
				return a.Span.SpanID < b.Span.SpanID
			})
		}
		sort.SliceStable(t.Nodes, func(i, j int) bool {
			a, b := t.Nodes[i], t.Nodes[j]
			if a.StartNs != b.StartNs {
				return a.StartNs < b.StartNs
			}
			return a.Span.SpanID < b.Span.SpanID
		})
		out = append(out, t)
	}
	return out
}

// hasCycle reports whether fewer than total nodes are reachable from root,
// which for a single-root, all-parents-present trace means a parent cycle.
func hasCycle(root *Node, total int) bool {
	seen := 0
	stack := []*Node{root}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		seen++
		if seen > total {
			return true
		}
		stack = append(stack, n.Children...)
	}
	return seen != total
}

// DurationNs returns the span duration.
func (n *Node) DurationNs() int64 { return n.EndNs - n.StartNs }

// IsError reports whether the span has error status.
func (n *Node) IsError() bool { return n.Span.StatusCode == model.StatusError }

// IsClient reports whether the span is a CLIENT span.
func (n *Node) IsClient() bool { return n.Span.Kind == model.KindClient }

// IsEntry reports whether the span is a request handled by its service.
func (n *Node) IsEntry() bool { return n.Span.IsEntry() }

// ServerChild returns the span in another service that handled this client
// call, or nil when none was recorded.
func (n *Node) ServerChild() *Node {
	for _, c := range n.Children {
		if c.Span.Service != n.Span.Service {
			return c
		}
	}
	return nil
}

// Callee names the service a client span called: the service of its paired
// child span, else its peer attribute. It is empty for non-client spans.
func (n *Node) Callee() string {
	if !n.IsClient() {
		return ""
	}
	if s := n.ServerChild(); s != nil {
		return s.Span.Service
	}
	return n.Span.PeerService
}

// IsTimeout reports whether a span ended because a deadline expired, judged
// from error.type and the status message.
func (n *Node) IsTimeout() bool {
	if !n.IsError() {
		return false
	}
	for _, text := range []string{n.Span.ErrorType, n.Span.StatusMessage} {
		l := strings.ToLower(text)
		if strings.Contains(l, "timeout") || strings.Contains(l, "deadline") || strings.Contains(l, "timed out") {
			return true
		}
	}
	return false
}

// HasErrorChild reports whether any direct child has error status.
func (n *Node) HasErrorChild() bool {
	for _, c := range n.Children {
		if c.IsError() {
			return true
		}
	}
	return false
}

// IsOriginError reports whether the span failed without any failed child,
// i.e. the error is first observed at this span rather than propagated.
func (n *Node) IsOriginError() bool { return n.IsError() && !n.HasErrorChild() }

// ExclusiveNs returns the span's self time: its duration minus the union of
// its children's intervals, clipped to the span. Children that outlive the
// span only count for the part that overlaps it.
func (n *Node) ExclusiveNs() int64 {
	covered := int64(0)
	cursor := n.StartNs
	for _, c := range n.Children { // already sorted by start
		start, end := max(c.StartNs, cursor), min(c.EndNs, n.EndNs)
		if end > start {
			covered += end - start
			cursor = end
		}
	}
	return max(n.DurationNs()-covered, 0)
}

// Unit is a step in a parent span's work: either a single non-client child,
// or a retry group of client attempts against the same callee.
type Unit struct {
	Attempts []*Node
	Callee   string
}

// First returns the first attempt.
func (u Unit) First() *Node { return u.Attempts[0] }

// Last returns the final attempt.
func (u Unit) Last() *Node { return u.Attempts[len(u.Attempts)-1] }

// StartNs returns when the unit began.
func (u Unit) StartNs() int64 { return u.First().StartNs }

// EndNs returns when the unit's last attempt ended.
func (u Unit) EndNs() int64 { return u.Last().EndNs }

// Failed reports whether the unit's final attempt failed.
func (u Unit) Failed() bool { return u.Last().IsError() }

// Units splits a span's children into units. Client spans to the same callee
// with the same name form one retry group when each carries a higher
// http.request.resend_count than the previous; everything else is its own unit.
func (n *Node) Units() []Unit {
	var units []Unit
	open := map[string]int{} // callee+name → index of the open retry group
	for _, c := range n.Children {
		if !c.IsClient() {
			units = append(units, Unit{Attempts: []*Node{c}})
			continue
		}
		callee := c.Callee()
		key := callee + "\x1f" + c.Span.Name
		if idx, ok := open[key]; ok && c.Span.ResendCount > units[idx].Last().Span.ResendCount {
			units[idx].Attempts = append(units[idx].Attempts, c)
			continue
		}
		units = append(units, Unit{Attempts: []*Node{c}, Callee: callee})
		open[key] = len(units) - 1
	}
	return units
}

// Retries counts client attempts after the first within every retry group of
// the subtree rooted at n.
func (n *Node) Retries() int {
	total := 0
	for _, u := range n.Units() {
		total += len(u.Attempts) - 1
	}
	for _, c := range n.Children {
		total += c.Retries()
	}
	return total
}
