// Package otlp decodes and validates OTLP trace export requests into
// TraceForge's normalised span model. All input is treated as untrusted.
package otlp

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"
	"unicode/utf8"

	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/TrulyAchyuth/TraceForge/internal/model"
)

// Content types accepted by the receiver.
const (
	ContentProtobuf = "application/x-protobuf"
	ContentJSON     = "application/json"
)

// Limits bound what a single span may carry. Values beyond a limit are
// truncated or dropped; they never cause the whole request to fail.
type Limits struct {
	MaxAttributes  int
	MaxKeyLen      int
	MaxValueLen    int
	MaxNameLen     int
	MaxEvents      int
	MaxFutureSkew  time.Duration
	MaxAge         time.Duration
	MaxSpanSeconds float64
}

// DefaultLimits are the limits applied when none are configured.
func DefaultLimits() Limits {
	return Limits{
		MaxAttributes:  64,
		MaxKeyLen:      128,
		MaxValueLen:    1024,
		MaxNameLen:     256,
		MaxEvents:      32,
		MaxFutureSkew:  10 * time.Minute,
		MaxAge:         24 * time.Hour,
		MaxSpanSeconds: 3600,
	}
}

// Rejection reasons reported in ingestion statistics.
const (
	RejectTraceID   = "invalid_trace_id"
	RejectSpanID    = "invalid_span_id"
	RejectTimestamp = "invalid_timestamp"
	RejectTooOld    = "too_old"
	RejectFuture    = "in_future"
)

// Result is the outcome of converting an export request.
type Result struct {
	Spans    []model.Span
	Rejected map[string]int
}

// RejectedCount returns the total number of rejected spans.
func (r Result) RejectedCount() int {
	n := 0
	for _, c := range r.Rejected {
		n += c
	}
	return n
}

// ErrUnsupportedContentType is returned for content types other than OTLP
// protobuf and OTLP JSON.
var ErrUnsupportedContentType = errors.New("unsupported content type")

// Unmarshal parses an OTLP ExportTraceServiceRequest body.
func Unmarshal(contentType string, body []byte) (*collectortrace.ExportTraceServiceRequest, error) {
	req := &collectortrace.ExportTraceServiceRequest{}
	switch contentType {
	case ContentProtobuf:
		if err := proto.Unmarshal(body, req); err != nil {
			return nil, fmt.Errorf("decode protobuf: %w", err)
		}
	case ContentJSON:
		opts := protojson.UnmarshalOptions{DiscardUnknown: true}
		if err := opts.Unmarshal(body, req); err != nil {
			return nil, fmt.Errorf("decode json: %w", err)
		}
		fixJSONIDs(req)
	default:
		return nil, ErrUnsupportedContentType
	}
	return req, nil
}

// fixJSONIDs repairs trace and span identifiers after protojson decoding.
//
// OTLP/JSON encodes IDs as hex strings, while protojson decodes every bytes
// field as base64. A hex string is also valid base64, so decoding silently
// yields the wrong bytes (24 instead of 16 for a trace ID). Re-encoding those
// bytes as base64 recovers the original hex text exactly, which is then
// decoded properly.
func fixJSONIDs(req *collectortrace.ExportTraceServiceRequest) {
	for _, rs := range req.GetResourceSpans() {
		for _, ss := range rs.GetScopeSpans() {
			for _, sp := range ss.GetSpans() {
				sp.TraceId = hexFromMisdecoded(sp.TraceId, 16)
				sp.SpanId = hexFromMisdecoded(sp.SpanId, 8)
				sp.ParentSpanId = hexFromMisdecoded(sp.ParentSpanId, 8)
			}
		}
	}
}

func hexFromMisdecoded(b []byte, want int) []byte {
	if len(b) == 0 {
		return b
	}
	text := base64.StdEncoding.EncodeToString(b)
	if len(text) != want*2 {
		return nil
	}
	out, err := hex.DecodeString(text)
	if err != nil {
		return nil
	}
	return out
}

// Convert validates the spans in req and converts them to the internal model.
// Invalid spans are counted by reason and skipped.
func Convert(req *collectortrace.ExportTraceServiceRequest, lim Limits, now time.Time) Result {
	res := Result{Rejected: map[string]int{}}
	for _, rs := range req.GetResourceSpans() {
		resource := attrMap(rs.GetResource().GetAttributes(), lim)
		service, _ := resource["service.name"].(string)
		if service == "" {
			service = "unknown_service"
		}
		service = truncate(service, lim.MaxKeyLen)
		for _, ss := range rs.GetScopeSpans() {
			for _, sp := range ss.GetSpans() {
				span, reason := convertSpan(sp, service, resource, lim, now)
				if reason != "" {
					res.Rejected[reason]++
					continue
				}
				res.Spans = append(res.Spans, span)
			}
		}
	}
	return res
}

func convertSpan(sp *tracepb.Span, service string, resource map[string]any, lim Limits, now time.Time) (model.Span, string) {
	if len(sp.GetTraceId()) != 16 || allZero(sp.GetTraceId()) {
		return model.Span{}, RejectTraceID
	}
	if len(sp.GetSpanId()) != 8 || allZero(sp.GetSpanId()) {
		return model.Span{}, RejectSpanID
	}
	parent := ""
	if p := sp.GetParentSpanId(); len(p) == 8 && !allZero(p) {
		parent = hex.EncodeToString(p)
	} else if len(p) != 0 && len(p) != 8 {
		return model.Span{}, RejectSpanID
	}
	startNs, endNs := sp.GetStartTimeUnixNano(), sp.GetEndTimeUnixNano()
	if startNs == 0 || endNs < startNs {
		return model.Span{}, RejectTimestamp
	}
	// Reject timestamps that cannot be represented or are clearly bogus
	// before converting, so arithmetic below cannot overflow.
	const maxUnixNano = uint64(1<<63 - 1)
	if startNs > maxUnixNano || endNs > maxUnixNano {
		return model.Span{}, RejectTimestamp
	}
	start := time.Unix(0, int64(startNs)).UTC()
	duration := int64(endNs - startNs)
	if float64(duration)/1e9 > lim.MaxSpanSeconds {
		return model.Span{}, RejectTimestamp
	}
	if start.After(now.Add(lim.MaxFutureSkew)) {
		return model.Span{}, RejectFuture
	}
	if start.Before(now.Add(-lim.MaxAge)) {
		return model.Span{}, RejectTooOld
	}

	attrs := attrMap(sp.GetAttributes(), lim)
	name := truncate(sp.GetName(), lim.MaxNameLen)
	if name == "" {
		name = "(unnamed)"
	}
	kind := int(sp.GetKind())
	if kind < model.KindUnspecified || kind > model.KindConsumer {
		kind = model.KindUnspecified
	}
	status := int(sp.GetStatus().GetCode())
	if status < model.StatusUnset || status > model.StatusError {
		status = model.StatusUnset
	}
	out := model.Span{
		TraceID:       hex.EncodeToString(sp.GetTraceId()),
		SpanID:        hex.EncodeToString(sp.GetSpanId()),
		ParentSpanID:  parent,
		Service:       service,
		Name:          name,
		Kind:          kind,
		Start:         start,
		DurationNs:    duration,
		StatusCode:    status,
		StatusMessage: truncate(sp.GetStatus().GetMessage(), lim.MaxValueLen),
		Attributes:    attrs,
		Resource:      resource,
	}
	out.PeerService = stringAttr(attrs, "peer.service")
	if out.PeerService == "" && kind == model.KindClient {
		out.PeerService = stringAttr(attrs, "server.address")
	}
	out.ErrorType = stringAttr(attrs, "error.type")
	out.ResendCount = intAttr(attrs, "http.request.resend_count")
	for i, ev := range sp.GetEvents() {
		if i >= lim.MaxEvents {
			break
		}
		if ev.GetTimeUnixNano() > maxUnixNano {
			continue
		}
		out.Events = append(out.Events, model.SpanEvent{
			Time:       time.Unix(0, int64(ev.GetTimeUnixNano())).UTC(),
			Name:       truncate(ev.GetName(), lim.MaxNameLen),
			Attributes: attrMap(ev.GetAttributes(), lim),
		})
	}
	return out, ""
}

func allZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

func stringAttr(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func intAttr(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case int64:
		if v < 0 || v > 1<<20 {
			return 0
		}
		return int(v)
	case float64:
		if v < 0 || v > 1<<20 {
			return 0
		}
		return int(v)
	case string:
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 1<<20 {
			return 0
		}
		return n
	}
	return 0
}

// truncate shortens s to at most max bytes without splitting a UTF-8 rune,
// and drops NUL bytes, which PostgreSQL text columns cannot store.
func truncate(s string, max int) string {
	if !utf8.ValidString(s) {
		s = string([]rune(s))
	}
	clean := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != 0 {
			clean = append(clean, s[i])
		}
	}
	s = string(clean)
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func attrMap(kvs []*commonpb.KeyValue, lim Limits) map[string]any {
	out := make(map[string]any, min(len(kvs), lim.MaxAttributes))
	for _, kv := range kvs {
		if len(out) >= lim.MaxAttributes {
			break
		}
		key := truncate(kv.GetKey(), lim.MaxKeyLen)
		if key == "" {
			continue
		}
		out[key] = anyValue(kv.GetValue(), lim, 0)
	}
	return out
}

// maxValueDepth bounds recursion through nested array and kvlist values.
const maxValueDepth = 4

func anyValue(v *commonpb.AnyValue, lim Limits, depth int) any {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return truncate(x.StringValue, lim.MaxValueLen)
	case *commonpb.AnyValue_BoolValue:
		return x.BoolValue
	case *commonpb.AnyValue_IntValue:
		return x.IntValue
	case *commonpb.AnyValue_DoubleValue:
		// NaN and infinities have no JSON representation; keep them as text.
		if math.IsNaN(x.DoubleValue) || math.IsInf(x.DoubleValue, 0) {
			return strconv.FormatFloat(x.DoubleValue, 'g', -1, 64)
		}
		return x.DoubleValue
	case *commonpb.AnyValue_BytesValue:
		return truncate(base64.StdEncoding.EncodeToString(x.BytesValue), lim.MaxValueLen)
	case *commonpb.AnyValue_ArrayValue:
		if depth >= maxValueDepth {
			return nil
		}
		vals := x.ArrayValue.GetValues()
		if len(vals) > lim.MaxAttributes {
			vals = vals[:lim.MaxAttributes]
		}
		arr := make([]any, 0, len(vals))
		for _, e := range vals {
			arr = append(arr, anyValue(e, lim, depth+1))
		}
		return arr
	case *commonpb.AnyValue_KvlistValue:
		if depth >= maxValueDepth {
			return nil
		}
		m := map[string]any{}
		for _, kv := range x.KvlistValue.GetValues() {
			if len(m) >= lim.MaxAttributes {
				break
			}
			m[truncate(kv.GetKey(), lim.MaxKeyLen)] = anyValue(kv.GetValue(), lim, depth+1)
		}
		return m
	}
	return nil
}
