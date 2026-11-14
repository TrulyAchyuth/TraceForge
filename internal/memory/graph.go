package memory

import (
	"sort"
	"strings"
)

// Node types in the Incident Memory Graph.
const (
	NodeIncident     = "incident"
	NodeService      = "service"
	NodeSignature    = "signature"
	NodePath         = "path"
	NodeIntervention = "intervention"
	NodeOutcome      = "outcome"
)

// Node is a graph vertex. Services, signatures, paths, interventions and
// outcomes are shared between incidents, which is what links them.
type Node struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Label     string `json:"label"`
	Simulated bool   `json:"simulated,omitempty"`
	Severity  string `json:"severity,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

// Edge links an incident to something it involved. Effect is set on
// intervention edges and is the simulated outcome recorded for that incident.
type Edge struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Kind   string `json:"kind"`
	Effect string `json:"effect,omitempty"`
	Note   string `json:"note,omitempty"`
}

// Graph is the whole memory as nodes and edges.
type Graph struct {
	Nodes []Node `json:"nodes"`
	Edges []Edge `json:"edges"`
}

// BuildGraph derives the graph from stored records only; it adds nothing.
func BuildGraph(records []Record) Graph {
	nodes := map[string]Node{}
	var edges []Edge
	add := func(n Node) {
		if old, ok := nodes[n.ID]; ok {
			// A shared node is simulated only if every incident using it is.
			old.Simulated = old.Simulated && n.Simulated
			nodes[n.ID] = old
			return
		}
		nodes[n.ID] = n
	}
	for _, r := range records {
		inc := "inc:" + r.Key
		add(Node{ID: inc, Type: NodeIncident, Label: r.Key, Simulated: r.Simulated, Severity: r.Severity, Detail: r.Title})
		for _, s := range r.Affected {
			add(Node{ID: "svc:" + s, Type: NodeService, Label: s, Simulated: r.Simulated})
			kind := "affected"
			if s == r.Primary {
				kind = "primary"
			}
			edges = append(edges, Edge{Source: inc, Target: "svc:" + s, Kind: kind})
		}
		seen := map[string]bool{}
		for _, s := range r.Signals {
			if !seen[s.Rule] {
				seen[s.Rule] = true
				add(Node{ID: "sig:" + s.Rule, Type: NodeSignature, Label: s.Rule, Simulated: r.Simulated})
				edges = append(edges, Edge{Source: inc, Target: "sig:" + s.Rule, Kind: "signature"})
			}
		}
		for _, h := range r.Hypotheses {
			if h.Rank == 1 && h.Kind != "undetermined" {
				id := "sig:" + h.Kind + "@" + h.Service
				add(Node{ID: id, Type: NodeSignature, Label: h.Kind + "@" + h.Service, Simulated: r.Simulated,
					Detail: "top-ranked hypothesis"})
				edges = append(edges, Edge{Source: inc, Target: id, Kind: "top hypothesis"})
			}
		}
		for _, e := range r.Edges {
			id := "path:" + e
			add(Node{ID: id, Type: NodePath, Label: strings.Replace(e, ">", " → ", 1), Simulated: r.Simulated})
			edges = append(edges, Edge{Source: inc, Target: id, Kind: "path"})
		}
		for _, iv := range r.Interventions {
			id := "int:" + iv.Detail
			add(Node{ID: id, Type: NodeIntervention, Label: iv.Detail, Simulated: r.Simulated, Detail: iv.Class})
			edges = append(edges, Edge{Source: inc, Target: id, Kind: "tried", Effect: iv.Effect, Note: iv.Summary})
		}
		out := "out:" + r.Recovery.Status + ":" + outcomeKind(r.Recovery.Cause)
		add(Node{ID: out, Type: NodeOutcome, Label: r.Recovery.Status + " — " + outcomeKind(r.Recovery.Cause), Simulated: r.Simulated})
		edges = append(edges, Edge{Source: inc, Target: out, Kind: "ended", Note: r.Recovery.Cause})
	}
	g := Graph{Nodes: make([]Node, 0, len(nodes)), Edges: edges}
	for _, n := range nodes {
		g.Nodes = append(g.Nodes, n)
	}
	sort.Slice(g.Nodes, func(i, j int) bool { return g.Nodes[i].ID < g.Nodes[j].ID })
	if g.Edges == nil {
		g.Edges = []Edge{}
	}
	return g
}

func outcomeKind(cause string) string {
	switch {
	case strings.HasPrefix(cause, "a known injected fault ended"):
		return "known fault ended"
	case strings.HasPrefix(cause, "resolved with no recorded cause"):
		return "no recorded cause"
	}
	return "cause not recorded"
}
