// Package replay implements Counterfactual Incident Replay (CIR): a
// deterministic simulation that recomputes the timings and outcomes of the
// traces captured for an incident under operator-chosen interventions.
//
// CIR is a telemetry simulation. It never re-executes application code, and
// its output is evidence of class "simulated", not proof of causation.
package replay

import (
	"errors"
	"fmt"
	"sort"

	"github.com/TrulyAchyuth/TraceForge/internal/model"
	"github.com/TrulyAchyuth/TraceForge/internal/tree"
)

// EngineVersion identifies the simulation model. It is stored with every run
// so results remain interpretable if the model changes.
const EngineVersion = "cir-1"

// Intervention types.
const (
	TypeScaleLatency = "scale_latency"
	TypeRemoveErrors = "remove_errors"
	TypeRetryPolicy  = "retry_policy"
	TypeSetTimeout   = "set_timeout"
)

// Latency intervention modes.
const (
	ModeFactor   = "factor"
	ModeBaseline = "baseline"
)

// Intervention is one change applied to the recorded incident.
type Intervention struct {
	Type string `json:"type"`
	// Service is the target of scale_latency and remove_errors.
	Service string `json:"service,omitempty"`
	// Mode selects how scale_latency derives its factor: "factor" uses
	// Factor; "baseline" uses median baseline self time / median incident
	// self time for the service (never above 1).
	Mode   string  `json:"mode,omitempty"`
	Factor float64 `json:"factor,omitempty"`
	// Caller and Callee select the edge for retry_policy and set_timeout.
	Caller string `json:"caller,omitempty"`
	Callee string `json:"callee,omitempty"`
	// MaxAttempts is the total attempts allowed by retry_policy (1 = no retries).
	MaxAttempts int `json:"max_attempts,omitempty"`
	// TimeoutMs is the per-attempt client timeout for set_timeout.
	TimeoutMs float64 `json:"timeout_ms,omitempty"`
}

// Scenario is a named set of interventions plus the simulation seed.
type Scenario struct {
	Name          string         `json:"name"`
	Interventions []Intervention `json:"interventions"`
	Seed          int64          `json:"seed"`
	// LatencyObjectiveMs is the request-latency objective used for the
	// "objective breach" outcome. Zero selects DefaultObjectiveMs.
	LatencyObjectiveMs float64 `json:"latency_objective_ms"`
}

// Validation bounds.
const (
	MaxInterventions   = 8
	MaxFactor          = 20.0
	MaxAttemptsLimit   = 10
	MaxTimeoutMs       = 120000.0
	DefaultObjectiveMs = 1000.0
	maxNameLen         = 120
)

// ValidationError lists every problem found in a scenario.
type ValidationError struct{ Problems []string }

func (e *ValidationError) Error() string {
	if len(e.Problems) == 1 {
		return "invalid replay scenario: " + e.Problems[0]
	}
	msg := "invalid replay scenario:"
	for _, p := range e.Problems {
		msg += " " + p + ";"
	}
	return msg
}

// IsValidation reports whether err is a scenario validation failure.
func IsValidation(err error) bool {
	var v *ValidationError
	return errors.As(err, &v)
}

type edgeKey struct{ caller, callee string }

// topology is what the dataset says exists: services and call edges.
type topology struct {
	services map[string]bool
	edges    map[edgeKey]bool
}

func topologyOf(traces []*tree.Trace) topology {
	t := topology{services: map[string]bool{}, edges: map[edgeKey]bool{}}
	for _, tr := range traces {
		for _, n := range tr.Nodes {
			t.services[n.Span.Service] = true
			if callee := n.Callee(); callee != "" {
				t.edges[edgeKey{n.Span.Service, callee}] = true
			}
		}
	}
	return t
}

// validate checks a scenario against the dataset topology.
func validate(sc Scenario, topo topology) error {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	if len(sc.Name) > maxNameLen {
		add("name must be at most %d characters", maxNameLen)
	}
	if len(sc.Interventions) == 0 {
		add("at least one intervention is required")
	}
	if len(sc.Interventions) > MaxInterventions {
		add("at most %d interventions are allowed", MaxInterventions)
	}
	if sc.LatencyObjectiveMs < 0 || sc.LatencyObjectiveMs > MaxTimeoutMs {
		add("latency_objective_ms must be between 0 and %.0f", MaxTimeoutMs)
	}
	seen := map[string]bool{}
	for i, iv := range sc.Interventions {
		label := fmt.Sprintf("intervention %d (%s)", i+1, iv.Type)
		var target string
		switch iv.Type {
		case TypeScaleLatency:
			target = iv.Service
			if !topo.services[iv.Service] {
				add("%s: service %q does not appear in the captured dataset", label, iv.Service)
			}
			switch iv.Mode {
			case ModeFactor, "":
				if iv.Factor < 0 || iv.Factor > MaxFactor {
					add("%s: factor must be between 0 and %.0f", label, MaxFactor)
				}
			case ModeBaseline:
			default:
				add("%s: mode must be %q or %q", label, ModeFactor, ModeBaseline)
			}
		case TypeRemoveErrors:
			target = iv.Service
			if !topo.services[iv.Service] {
				// A service that was down emits no spans but is still a callee.
				known := false
				for e := range topo.edges {
					if e.callee == iv.Service {
						known = true
						break
					}
				}
				if !known {
					add("%s: service %q does not appear in the captured dataset", label, iv.Service)
				}
			}
		case TypeRetryPolicy, TypeSetTimeout:
			target = iv.Caller + "→" + iv.Callee
			if !topo.edges[edgeKey{iv.Caller, iv.Callee}] {
				add("%s: no calls from %q to %q appear in the captured dataset", label, iv.Caller, iv.Callee)
			}
			if iv.Type == TypeRetryPolicy && (iv.MaxAttempts < 1 || iv.MaxAttempts > MaxAttemptsLimit) {
				add("%s: max_attempts must be between 1 and %d", label, MaxAttemptsLimit)
			}
			if iv.Type == TypeSetTimeout && (iv.TimeoutMs < 1 || iv.TimeoutMs > MaxTimeoutMs) {
				add("%s: timeout_ms must be between 1 and %.0f", label, MaxTimeoutMs)
			}
		default:
			add("intervention %d: unknown type %q", i+1, iv.Type)
			continue
		}
		key := iv.Type + "|" + target
		if seen[key] {
			add("%s: duplicate intervention for %s", label, target)
		}
		seen[key] = true
	}
	if len(problems) > 0 {
		return &ValidationError{Problems: problems}
	}
	return nil
}

// Options lists what a dataset supports, for building scenarios in the UI.
type Options struct {
	Services []string     `json:"services"`
	Edges    []EdgeOption `json:"edges"`
}

// EdgeOption describes an edge and what the model inferred about it.
type EdgeOption struct {
	Caller              string  `json:"caller"`
	Callee              string  `json:"callee"`
	ObservedMaxAttempts int     `json:"observed_max_attempts"`
	InferredTimeoutMs   float64 `json:"inferred_timeout_ms"`
}

// OptionsFor reports the services and edges available as intervention
// targets in a dataset, with the retry and timeout behaviour inferred for
// each edge.
func OptionsFor(ds *model.Dataset) Options {
	traces := tree.Build(ds.Spans)
	topo := topologyOf(traces)
	m := buildModel(traces)
	opts := Options{Services: []string{}, Edges: []EdgeOption{}}
	for s := range topo.services {
		opts.Services = append(opts.Services, s)
	}
	sort.Strings(opts.Services)
	for e := range topo.edges {
		eo := EdgeOption{Caller: e.caller, Callee: e.callee, ObservedMaxAttempts: 1}
		if em := m.edges[e]; em != nil {
			eo.ObservedMaxAttempts = max(em.maxAttempts, 1)
			eo.InferredTimeoutMs = float64(em.timeoutNs) / 1e6
		}
		opts.Edges = append(opts.Edges, eo)
	}
	sort.Slice(opts.Edges, func(i, j int) bool {
		if opts.Edges[i].Caller != opts.Edges[j].Caller {
			return opts.Edges[i].Caller < opts.Edges[j].Caller
		}
		return opts.Edges[i].Callee < opts.Edges[j].Callee
	})
	return opts
}
