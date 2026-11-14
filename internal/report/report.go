// Package report renders incident investigation reports from stored data.
package report

import (
	"bytes"
	"encoding/json"
	"fmt"
	htmltemplate "html/template"
	"strings"
	"text/template"
	"time"

	"github.com/TrulyAchyuth/TraceForge/internal/investigate"
	"github.com/TrulyAchyuth/TraceForge/internal/model"
	"github.com/TrulyAchyuth/TraceForge/internal/replay"
)

// ReplaySection is one stored replay run with its parsed result.
type ReplaySection struct {
	Run    model.ReplayRun `json:"run"`
	Result *replay.Result  `json:"result"`
}

// Recommendation is a suggested follow-up and the stored finding behind it.
type Recommendation struct {
	Action string `json:"action"`
	Basis  string `json:"basis"`
}

// Report is the complete content of an incident report.
type Report struct {
	GeneratedAt      time.Time                    `json:"generated_at"`
	Incident         model.Incident               `json:"incident"`
	DurationText     string                       `json:"duration"`
	ExecutiveSummary []string                     `json:"executive_summary"`
	Dataset          *model.Dataset               `json:"dataset,omitempty"`
	Services         []investigate.ServiceProfile `json:"services"`
	Edges            []investigate.EdgeProfile    `json:"edges"`
	KnownFaults      []investigate.KnownFault     `json:"known_faults"`
	Hypotheses       []model.Hypothesis           `json:"hypotheses"`
	Timeline         []model.Event                `json:"timeline"`
	Replays          []ReplaySection              `json:"replays"`
	Recommendations  []Recommendation             `json:"recommendations"`
	Limitations      []string                     `json:"limitations"`
	Methodology      []string                     `json:"methodology"`
	Similar          []SimilarSection             `json:"similar_incidents"`
}

// SimilarSection is one remembered incident that resembles this one. Overlap
// is a weighted feature overlap, not a probability; interventions were tested
// on that other incident's data.
type SimilarSection struct {
	Key           string   `json:"key"`
	Title         string   `json:"title"`
	Simulated     bool     `json:"simulated_example"`
	Overlap       float64  `json:"overlap"`
	Shared        []string `json:"shared_features"`
	Differences   []string `json:"differences"`
	Interventions []string `json:"interventions"`
}

// Build assembles a report from an investigation view and the incident's
// stored replay runs (with results). Every statement derives from that data.
func Build(v *investigate.View, replays []model.ReplayRun, now time.Time) (*Report, error) {
	r := &Report{GeneratedAt: now.UTC(), Incident: v.Incident, Dataset: v.Dataset, KnownFaults: v.KnownFaults,
		Hypotheses: v.Hypotheses, Timeline: v.Timeline, Methodology: v.Methodology,
		Services: []investigate.ServiceProfile{}, Edges: []investigate.EdgeProfile{}}
	if v.Analysis != nil {
		r.Services, r.Edges = v.Analysis.Services, v.Analysis.Edges
	}
	for _, run := range replays {
		var res replay.Result
		if err := json.Unmarshal(run.Result, &res); err != nil {
			return nil, fmt.Errorf("decode replay %d: %w", run.ID, err)
		}
		run.Result = nil
		r.Replays = append(r.Replays, ReplaySection{Run: run, Result: &res})
	}
	end := now
	if v.Incident.ResolvedAt != nil {
		end = *v.Incident.ResolvedAt
	}
	r.DurationText = end.Sub(v.Incident.StartedAt).Round(time.Second).String()
	r.ExecutiveSummary = summary(r, v)
	r.Recommendations = recommendations(r)
	r.Limitations = limitations(r, v)
	return r, nil
}

func ts(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05 UTC") }

func pct(v float64) string { return fmt.Sprintf("%.1f%%", v*100) }

func summary(r *Report, v *investigate.View) []string {
	in := r.Incident
	var out []string
	state := "It is still " + in.Status + "."
	if in.ResolvedAt != nil {
		state = fmt.Sprintf("It was %s at %s, %s after its estimated start.", in.Status, ts(*in.ResolvedAt), r.DurationText)
	}
	out = append(out, fmt.Sprintf("%s (%s severity) was detected at %s on service %s by the %s rule. %s",
		in.Key, in.Severity, ts(in.DetectedAt), in.PrimaryService, in.TriggerRule, state))
	out = append(out, fmt.Sprintf("%d detection signals fired across %d services: %s.",
		len(in.Signals), len(in.AffectedServices), strings.Join(in.AffectedServices, ", ")))
	if v.Analysis != nil && v.Analysis.IncidentRequests > 0 {
		out = append(out, fmt.Sprintf("Of %d sampled requests during the incident, %d (%s) failed or were markedly slower than baseline.",
			v.Analysis.IncidentRequests, v.Analysis.ImpactedRequests,
			pct(float64(v.Analysis.ImpactedRequests)/float64(v.Analysis.IncidentRequests))))
	}
	switch {
	case r.Dataset == nil:
		out = append(out, "No telemetry dataset has been captured for this incident, so no hypotheses or replays are available.")
	case len(r.Hypotheses) > 0 && r.Hypotheses[0].Kind != investigate.KindUndetermined:
		h := r.Hypotheses[0]
		out = append(out, fmt.Sprintf("The highest-ranked hypothesis is \"%s\" with an evidence score of %d out of %d. This is a hypothesis supported by the evidence listed below, not an established root cause.",
			h.Title, h.Score, h.MaxScore))
	default:
		out = append(out, "The analysis did not isolate a service whose own behaviour changed; no origin hypothesis is offered.")
	}
	for _, f := range r.KnownFaults {
		match := "The top-ranked hypothesis does not name one of its targets."
		if len(r.Hypotheses) > 0 && r.Hypotheses[0].MatchesKnownFault {
			match = "The top-ranked hypothesis names one of its targets."
		}
		out = append(out, fmt.Sprintf("Observed fact: failure-lab experiment \"%s\" targeting %s was running during the incident. %s",
			f.Name, strings.Join(f.Targets, ", "), match))
	}
	if len(r.Replays) == 0 {
		out = append(out, "No counterfactual replay has been run.")
	} else {
		latest := r.Replays[0]
		s := latest.Result.Summary
		out = append(out, fmt.Sprintf("%d counterfactual replay(s) were run. In the most recent (\"%s\"), the simulated request error rate is %s against %s recorded, and simulated p95 latency is %.0f ms against %.0f ms recorded. These are simulation results, not measurements.",
			len(r.Replays), latest.Run.Name, pct(s.Counterfactual.ErrorRate), pct(s.Original.ErrorRate), s.Counterfactual.P95Ms, s.Original.P95Ms))
	}
	return out
}

// recommendations maps the leading hypotheses to standard follow-up actions.
// They are generic guidance keyed on hypothesis type, not findings.
func recommendations(r *Report) []Recommendation {
	var out []Recommendation
	seen := map[string]bool{}
	add := func(action, basis string) {
		if !seen[action] {
			seen[action] = true
			out = append(out, Recommendation{Action: action, Basis: basis})
		}
	}
	for i, h := range r.Hypotheses {
		if i >= 3 || h.Score <= 0 {
			break
		}
		basis := fmt.Sprintf("Hypothesis #%d: %s (score %d/%d)", h.Rank, h.Title, h.Score, h.MaxScore)
		switch h.Kind {
		case investigate.KindLatencyOrigin:
			add(fmt.Sprintf("Examine what %s was doing during the incident window (deploys, resource saturation, slow queries or dependencies not covered by tracing).", h.Service), basis)
			add(fmt.Sprintf("Confirm that callers of %s have timeouts and retry budgets appropriate to its normal latency.", h.Service), basis)
		case investigate.KindErrorOrigin:
			add(fmt.Sprintf("Review %s's error logs and recent changes for the incident window to find the failing code path.", h.Service), basis)
		case investigate.KindUnavailable:
			add(fmt.Sprintf("Establish why %s stopped responding (process, host or network) and whether health checks detected it.", h.Service), basis)
			add(fmt.Sprintf("Decide how callers of %s should degrade when it is unreachable.", h.Service), basis)
		case investigate.KindRetryAmplify:
			add(fmt.Sprintf("Review the retry policy from %s to %s: attempt count, backoff with jitter, and a retry budget or circuit breaker.", h.Service, h.Peer), basis)
		case investigate.KindTrafficSurge:
			add(fmt.Sprintf("Identify the source of the additional traffic into %s and whether rate limiting or load shedding applies.", h.Service), basis)
		}
	}
	for _, rs := range r.Replays {
		s := rs.Result.Summary
		if s.Counterfactual.ErrorRate < s.Original.ErrorRate || s.Counterfactual.P95Ms < s.Original.P95Ms {
			add(fmt.Sprintf("Validate the change simulated in replay \"%s\" in a test environment before relying on it; the replay is a model, not a measurement.", rs.Run.Name),
				fmt.Sprintf("Replay #%d on dataset v%d", rs.Run.ID, rs.Run.DatasetVersion))
		}
	}
	if len(out) == 0 {
		add("Gather more telemetry (longer baseline, missing instrumentation) and re-run the analysis.", "No scored hypothesis is available")
	}
	return out
}

func limitations(r *Report, v *investigate.View) []string {
	out := []string{
		"Hypotheses are ranked by an additive evidence score. The score orders candidates; it is not a probability and no hypothesis in this report is a proven root cause.",
		"All analysis is derived from trace spans. Causes that leave no trace in span timing or status (for example host-level faults on an uninstrumented dependency) cannot be identified.",
		"Counterfactual replay results are simulated. They show how the recorded traces change under a documented model; they do not demonstrate what would have happened in the real system.",
	}
	if r.Dataset != nil {
		m := r.Dataset.Meta
		out = append(out, fmt.Sprintf("The dataset holds a uniform sample of %d of %d baseline traces and %d of %d incident traces. Statistics from the sample carry sampling error.",
			m.BaselineTracesSampled, m.BaselineTracesAvailable, m.IncidentTracesSampled, m.IncidentTracesAvailable))
		if !v.IntegrityOK {
			out = append(out, "WARNING: the stored dataset does not match the checksum recorded at capture. Findings derived from it should not be trusted.")
		}
	}
	if v.Analysis != nil && v.Analysis.TracesIncomplete > 0 {
		out = append(out, fmt.Sprintf("%d sampled traces were incomplete (missing spans) and were excluded from analysis.", v.Analysis.TracesIncomplete))
	}
	return out
}

var funcs = map[string]any{
	"ts":  ts,
	"pct": pct,
	"ms":  func(v float64) string { return fmt.Sprintf("%.0f ms", v) },
	"tsp": func(t *time.Time) string {
		if t == nil {
			return "—"
		}
		return ts(*t)
	},
	"join": strings.Join,
	// oneline keeps operator-supplied text from breaking Markdown structure.
	"oneline": func(s string) string {
		return strings.ReplaceAll(strings.Join(strings.Fields(s), " "), "|", "\\|")
	},
	"upper": strings.ToUpper,
	"short": func(s string) string {
		if len(s) > 12 {
			return s[:12]
		}
		return s
	},
	"signed": func(v int) string { return fmt.Sprintf("%+d", v) },
	"changed": func(b bool) string {
		if b {
			return "changed"
		}
		return "unchanged"
	},
	"fval": func(f replay.Finding, v float64) string {
		switch f.Unit {
		case "ratio":
			return pct(v)
		case "ms":
			return fmt.Sprintf("%.0f ms", v)
		}
		return fmt.Sprintf("%.0f", v)
	},
	"describe": describeIntervention,
	"interventions": func(raw json.RawMessage) []replay.Intervention {
		var ivs []replay.Intervention
		_ = json.Unmarshal(raw, &ivs)
		return ivs
	},
}

func describeIntervention(iv replay.Intervention) string {
	switch iv.Type {
	case replay.TypeScaleLatency:
		if iv.Mode == replay.ModeBaseline {
			return fmt.Sprintf("Restore %s self time to its baseline level", iv.Service)
		}
		return fmt.Sprintf("Scale %s self time by ×%.2f", iv.Service, iv.Factor)
	case replay.TypeRemoveErrors:
		return fmt.Sprintf("Remove errors originating in %s", iv.Service)
	case replay.TypeRetryPolicy:
		return fmt.Sprintf("Limit %s → %s to %d attempt(s)", iv.Caller, iv.Callee, iv.MaxAttempts)
	case replay.TypeSetTimeout:
		return fmt.Sprintf("Set %s → %s timeout to %.0f ms", iv.Caller, iv.Callee, iv.TimeoutMs)
	}
	return iv.Type
}

const markdownTemplate = `# Incident report: {{.Incident.Key}} — {{.Incident.Title}}

Generated {{ts .GeneratedAt}} by TraceForge from stored investigation data.

## Executive summary
{{range .ExecutiveSummary}}
- {{.}}{{end}}

## Incident details

| Field | Value |
|---|---|
| Identifier | {{.Incident.Key}} |
| Severity | {{.Incident.Severity}} |
| Status | {{.Incident.Status}} |
| Triggering rule | {{.Incident.TriggerRule}} |
| Primary service | {{.Incident.PrimaryService}} |
| Estimated start | {{ts .Incident.StartedAt}} |
| Detected | {{ts .Incident.DetectedAt}} |
| Resolved | {{tsp .Incident.ResolvedAt}} |
| Duration | {{.DurationText}} |

## Affected services

{{join .Incident.AffectedServices ", "}}

### Detection signals

| Rule | Service | Peer | Severity | Peak | Baseline | Threshold | Unit | First seen | Last seen |
|---|---|---|---|---|---|---|---|---|---|
{{range .Incident.Signals}}| {{.Rule}} | {{.Service}} | {{.Peer}} | {{.Severity}} | {{printf "%.3g" .PeakValue}} | {{printf "%.3g" .Baseline}} | {{printf "%.3g" .Threshold}} | {{.Unit}} | {{ts .FirstSeen}} | {{ts .LastSeen}} |
{{end}}
{{if .Services}}### Service comparison (sampled traces, baseline → incident)

| Service | Requests | p95 latency | Self-time p95 | Error rate | Origin-error rate |
|---|---|---|---|---|---|
{{range .Services}}| {{.Service}} | {{.BaselineRequests}} → {{.IncidentRequests}} | {{ms .BaselineP95Ms}} → {{ms .IncidentP95Ms}} | {{ms .BaselineSelfP95Ms}} → {{ms .IncidentSelfP95Ms}} | {{pct .BaselineErrorRate}} → {{pct .IncidentErrorRate}} | {{pct .BaselineOriginRate}} → {{pct .IncidentOriginRate}} |
{{end}}{{end}}
## Event timeline

| Time | Kind | Event |
|---|---|---|
{{range .Timeline}}| {{ts .Time}} | {{.Kind}} | {{oneline .Title}} |
{{end}}
## Investigation hypotheses

Evidence is labelled by class: **observed** (measured directly), **correlation** (statistical association), **inference** (reasoned from structure). The evidence score ranks hypotheses; it is not a probability.
{{if .KnownFaults}}
### Known fault injections (observed facts, not used for ranking)
{{range .KnownFaults}}
- Experiment #{{.ExperimentID}} "{{.Name}}" targeting {{join .Targets ", "}}, started {{ts .StartedAt}}, ended {{tsp .EndedAt}}.{{end}}
{{end}}{{range .Hypotheses}}
### #{{.Rank}} {{.Title}} — evidence score {{.Score}}/{{.MaxScore}}{{if .MatchesKnownFault}} (names a known fault target){{end}}

{{.Statement}}
{{range .Evidence}}
- **{{.Polarity}} · {{.Class}} · {{signed .Points}}** ({{.Check}}): {{.Text}}{{if .TraceIDs}} Example traces: {{range $i, $t := .TraceIDs}}{{if $i}}, {{end}}` + "`{{$t}}`" + `{{end}}.{{end}}{{end}}
{{end}}
## Counterfactual replay results
{{if not .Replays}}
No replay has been run for this incident.
{{end}}{{range .Replays}}
### Replay #{{.Run.ID}}: {{.Run.Name}}

Class of evidence: **simulated**. Dataset v{{.Run.DatasetVersion}} (checksum ` + "`{{short .Run.DatasetChecksum}}…`" + `), seed {{.Run.Seed}}, engine {{.Run.EngineVersion}}, run {{ts .Run.CreatedAt}}.

Interventions:
{{range interventions .Run.Interventions}}
- {{describe .}}{{end}}

| Outcome | Recorded | Simulated |
|---|---|---|
| Requests replayed | {{.Result.Summary.Original.Count}} | {{.Result.Summary.Counterfactual.Count}} |
| Error rate | {{pct .Result.Summary.Original.ErrorRate}} | {{pct .Result.Summary.Counterfactual.ErrorRate}} |
| p50 latency | {{ms .Result.Summary.Original.P50Ms}} | {{ms .Result.Summary.Counterfactual.P50Ms}} |
| p95 latency | {{ms .Result.Summary.Original.P95Ms}} | {{ms .Result.Summary.Counterfactual.P95Ms}} |
| p99 latency | {{ms .Result.Summary.Original.P99Ms}} | {{ms .Result.Summary.Counterfactual.P99Ms}} |
| Retries | {{.Result.Summary.OriginalRetries}} | {{.Result.Summary.CounterfactualRetries}} |
| Objective breaches (error or > {{ms .Result.Summary.ObjectiveMs}}) | {{pct .Result.Summary.OriginalBreachRate}} | {{pct .Result.Summary.CounterfactualBreachRt}} |

Outcome by scope:

| Scope | Metric | Recorded | Simulated | Result |
|---|---|---|---|---|
{{range .Result.Findings}}| {{.Scope}} | {{.Metric}} | {{fval . .Original}} | {{fval . .Counterfactual}} | {{changed .Changed}} |
{{end}}
Model self-check: {{.Result.Diagnostics.IdentityMismatches}} of {{.Result.Diagnostics.IdentityChecked}} traces were not reproduced exactly with no intervention.

Limitations of this replay:
{{range .Result.Limitations}}
- {{.}}{{end}}

Conclusions this replay does not support:
{{range .Result.Unsupported}}
- {{.}}{{end}}
{{end}}
## Investigation notes
{{if not .Incident.Notes}}
No notes recorded.
{{end}}{{range .Incident.Notes}}
- {{ts .CreatedAt}} — {{oneline .Author}}: {{oneline .Body}}{{end}}

## Recommended follow-up actions

These are standard follow-ups for the type of hypothesis named as their basis. They are suggestions, not findings.
{{range .Recommendations}}
- {{.Action}} _(Basis: {{.Basis}})_{{end}}

## Similar past incidents (Incident Memory)

Matched by weighted overlap of observable features (method mem-1). This is correlation between incidents, not evidence of a shared cause, and interventions below were tested on other data.
{{if not .Similar}}
No similar incident is stored in memory.
{{end}}{{range .Similar}}
### {{.Key}} — {{.Title}}{{if .Simulated}} [SIMULATED EXAMPLE]{{end}}
- Feature overlap: {{printf "%.3f" .Overlap}}; shared: {{join .Shared ", "}}
{{range .Differences}}- Difference: {{.}}
{{end}}{{range .Interventions}}- Intervention: {{.}}
{{end}}{{end}}
## Limitations and assumptions
{{range .Limitations}}
- {{.}}{{end}}

### Ranking methodology
{{range .Methodology}}
- {{.}}{{end}}
`

var mdTmpl = template.Must(template.New("md").Funcs(funcs).Parse(markdownTemplate))

// Markdown renders the report as Markdown.
func (r *Report) Markdown() ([]byte, error) {
	var buf bytes.Buffer
	if err := mdTmpl.Execute(&buf, r); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// JSON renders the report as indented JSON.
func (r *Report) JSON() ([]byte, error) { return json.MarshalIndent(r, "", "  ") }

const htmlTemplate = `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<title>{{.Incident.Key}} incident report</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>
body{font:15px/1.55 -apple-system,BlinkMacSystemFont,"Segoe UI",Helvetica,Arial,sans-serif;color:#1b1f24;max-width:960px;margin:40px auto;padding:0 24px}
h1{font-size:26px;margin:0 0 4px}h2{font-size:19px;margin:36px 0 10px;padding-bottom:6px;border-bottom:1px solid #d5dae0}h3{font-size:16px;margin:24px 0 8px}
table{border-collapse:collapse;width:100%;margin:10px 0;font-size:13.5px}th,td{border:1px solid #d5dae0;padding:6px 9px;text-align:left;vertical-align:top}th{background:#f3f5f7}
.meta{color:#5b6570}.tag{display:inline-block;padding:1px 7px;border-radius:3px;font-size:12px;font-weight:600;border:1px solid #c4cad1;margin-right:4px}
.supporting{border-color:#2b7a4b;color:#1f6a3d}.contradicting{border-color:#b3372f;color:#a12d26}.neutral{color:#5b6570}
.sim{background:#fff7e0;border:1px solid #e3c766;padding:8px 12px;border-radius:4px}
code{font:12.5px ui-monospace,SFMono-Regular,Menlo,monospace;background:#f3f5f7;padding:1px 4px;border-radius:3px}
@media print{body{margin:0;max-width:none}h2{break-after:avoid}table{break-inside:avoid}}
</style></head><body>
<h1>Incident report: {{.Incident.Key}} — {{.Incident.Title}}</h1>
<p class="meta">Generated {{ts .GeneratedAt}} by TraceForge from stored investigation data.</p>

<h2>Executive summary</h2><ul>{{range .ExecutiveSummary}}<li>{{.}}</li>{{end}}</ul>

<h2>Incident details</h2>
<table>
<tr><th>Identifier</th><td>{{.Incident.Key}}</td><th>Severity</th><td>{{.Incident.Severity}}</td></tr>
<tr><th>Status</th><td>{{.Incident.Status}}</td><th>Triggering rule</th><td>{{.Incident.TriggerRule}}</td></tr>
<tr><th>Estimated start</th><td>{{ts .Incident.StartedAt}}</td><th>Detected</th><td>{{ts .Incident.DetectedAt}}</td></tr>
<tr><th>Resolved</th><td>{{tsp .Incident.ResolvedAt}}</td><th>Duration</th><td>{{.DurationText}}</td></tr>
</table>

<h2>Affected services</h2><p>{{join .Incident.AffectedServices ", "}}</p>
<h3>Detection signals</h3>
<table><tr><th>Rule</th><th>Service</th><th>Peer</th><th>Severity</th><th>Peak</th><th>Baseline</th><th>Threshold</th><th>Unit</th><th>First seen</th></tr>
{{range .Incident.Signals}}<tr><td>{{.Rule}}</td><td>{{.Service}}</td><td>{{.Peer}}</td><td>{{.Severity}}</td><td>{{printf "%.3g" .PeakValue}}</td><td>{{printf "%.3g" .Baseline}}</td><td>{{printf "%.3g" .Threshold}}</td><td>{{.Unit}}</td><td>{{ts .FirstSeen}}</td></tr>{{end}}</table>
{{if .Services}}<h3>Service comparison (sampled traces, baseline → incident)</h3>
<table><tr><th>Service</th><th>Requests</th><th>p95 latency</th><th>Self-time p95</th><th>Error rate</th><th>Origin-error rate</th></tr>
{{range .Services}}<tr><td>{{.Service}}</td><td>{{.BaselineRequests}} → {{.IncidentRequests}}</td><td>{{ms .BaselineP95Ms}} → {{ms .IncidentP95Ms}}</td><td>{{ms .BaselineSelfP95Ms}} → {{ms .IncidentSelfP95Ms}}</td><td>{{pct .BaselineErrorRate}} → {{pct .IncidentErrorRate}}</td><td>{{pct .BaselineOriginRate}} → {{pct .IncidentOriginRate}}</td></tr>{{end}}</table>{{end}}

<h2>Event timeline</h2>
<table><tr><th>Time</th><th>Kind</th><th>Event</th></tr>
{{range .Timeline}}<tr><td>{{ts .Time}}</td><td>{{.Kind}}</td><td>{{.Title}}</td></tr>{{end}}</table>

<h2>Investigation hypotheses</h2>
<p>Evidence is labelled by class: <b>observed</b> (measured directly), <b>correlation</b> (statistical association), <b>inference</b> (reasoned from structure). The evidence score ranks hypotheses; it is not a probability.</p>
{{if .KnownFaults}}<h3>Known fault injections (observed facts, not used for ranking)</h3><ul>
{{range .KnownFaults}}<li>Experiment #{{.ExperimentID}} "{{.Name}}" targeting {{join .Targets ", "}}, started {{ts .StartedAt}}, ended {{tsp .EndedAt}}.</li>{{end}}</ul>{{end}}
{{range .Hypotheses}}<h3>#{{.Rank}} {{.Title}} — evidence score {{.Score}}/{{.MaxScore}}{{if .MatchesKnownFault}} (names a known fault target){{end}}</h3>
<p>{{.Statement}}</p><ul>
{{range .Evidence}}<li><span class="tag {{.Polarity}}">{{.Polarity}}</span><span class="tag">{{.Class}}</span><span class="tag">{{signed .Points}}</span> {{.Text}}{{if .TraceIDs}} Example traces: {{range $i, $t := .TraceIDs}}{{if $i}}, {{end}}<code>{{$t}}</code>{{end}}.{{end}}</li>{{end}}</ul>{{end}}

<h2>Counterfactual replay results</h2>
{{if not .Replays}}<p>No replay has been run for this incident.</p>{{end}}
{{range .Replays}}<h3>Replay #{{.Run.ID}}: {{.Run.Name}}</h3>
<p class="sim">Class of evidence: <b>simulated</b>. Dataset v{{.Run.DatasetVersion}} (checksum <code>{{short .Run.DatasetChecksum}}…</code>), seed {{.Run.Seed}}, engine {{.Run.EngineVersion}}, run {{ts .Run.CreatedAt}}.</p>
<p>Interventions:</p><ul>{{range interventions .Run.Interventions}}<li>{{describe .}}</li>{{end}}</ul>
<table><tr><th>Outcome</th><th>Recorded</th><th>Simulated</th></tr>
<tr><td>Requests replayed</td><td>{{.Result.Summary.Original.Count}}</td><td>{{.Result.Summary.Counterfactual.Count}}</td></tr>
<tr><td>Error rate</td><td>{{pct .Result.Summary.Original.ErrorRate}}</td><td>{{pct .Result.Summary.Counterfactual.ErrorRate}}</td></tr>
<tr><td>p50 latency</td><td>{{ms .Result.Summary.Original.P50Ms}}</td><td>{{ms .Result.Summary.Counterfactual.P50Ms}}</td></tr>
<tr><td>p95 latency</td><td>{{ms .Result.Summary.Original.P95Ms}}</td><td>{{ms .Result.Summary.Counterfactual.P95Ms}}</td></tr>
<tr><td>p99 latency</td><td>{{ms .Result.Summary.Original.P99Ms}}</td><td>{{ms .Result.Summary.Counterfactual.P99Ms}}</td></tr>
<tr><td>Retries</td><td>{{.Result.Summary.OriginalRetries}}</td><td>{{.Result.Summary.CounterfactualRetries}}</td></tr>
<tr><td>Objective breaches (error or &gt; {{ms .Result.Summary.ObjectiveMs}})</td><td>{{pct .Result.Summary.OriginalBreachRate}}</td><td>{{pct .Result.Summary.CounterfactualBreachRt}}</td></tr></table>
<table><tr><th>Scope</th><th>Metric</th><th>Recorded</th><th>Simulated</th><th>Result</th></tr>
{{range .Result.Findings}}<tr><td>{{.Scope}}</td><td>{{.Metric}}</td><td>{{fval . .Original}}</td><td>{{fval . .Counterfactual}}</td><td>{{changed .Changed}}</td></tr>{{end}}</table>
<p>Model self-check: {{.Result.Diagnostics.IdentityMismatches}} of {{.Result.Diagnostics.IdentityChecked}} traces were not reproduced exactly with no intervention.</p>
<p>Limitations of this replay:</p><ul>{{range .Result.Limitations}}<li>{{.}}</li>{{end}}</ul>
<p>Conclusions this replay does not support:</p><ul>{{range .Result.Unsupported}}<li>{{.}}</li>{{end}}</ul>{{end}}

<h2>Investigation notes</h2>
{{if not .Incident.Notes}}<p>No notes recorded.</p>{{end}}<ul>{{range .Incident.Notes}}<li>{{ts .CreatedAt}} — {{.Author}}: {{.Body}}</li>{{end}}</ul>

<h2>Recommended follow-up actions</h2>
<p>These are standard follow-ups for the type of hypothesis named as their basis. They are suggestions, not findings.</p>
<ul>{{range .Recommendations}}<li>{{.Action}} <span class="meta">(Basis: {{.Basis}})</span></li>{{end}}</ul>

<h2>Similar past incidents (Incident Memory)</h2>
<p>Matched by weighted overlap of observable features (method mem-1). This is correlation between incidents, not evidence of a shared cause; interventions were tested on other data.</p>
{{if not .Similar}}<p>No similar incident is stored in memory.</p>{{end}}
{{range .Similar}}<h3>{{.Key}} — {{.Title}}{{if .Simulated}} <span class="tag">SIMULATED EXAMPLE</span>{{end}}</h3>
<p>Feature overlap {{printf "%.3f" .Overlap}}. Shared: {{join .Shared ", "}}</p>
<ul>{{range .Differences}}<li>Difference: {{.}}</li>{{end}}{{range .Interventions}}<li>Intervention: {{.}}</li>{{end}}</ul>{{end}}

<h2>Limitations and assumptions</h2><ul>{{range .Limitations}}<li>{{.}}</li>{{end}}</ul>
<h3>Ranking methodology</h3><ul>{{range .Methodology}}<li>{{.}}</li>{{end}}</ul>
</body></html>
`

var htmlTmpl = htmltemplate.Must(htmltemplate.New("html").Funcs(funcs).Parse(htmlTemplate))

// HTML renders the report as a self-contained, printable HTML document.
func (r *Report) HTML() ([]byte, error) {
	var buf bytes.Buffer
	if err := htmlTmpl.Execute(&buf, r); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
