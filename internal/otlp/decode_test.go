package otlp

import (
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

var now = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

func span(trace, id []byte, start, end time.Time) *tracepb.Span {
	return &tracepb.Span{TraceId: trace, SpanId: id, Name: "GET /x", Kind: tracepb.Span_SPAN_KIND_SERVER,
		StartTimeUnixNano: uint64(start.UnixNano()), EndTimeUnixNano: uint64(end.UnixNano())}
}

func request(service string, spans ...*tracepb.Span) *collectortrace.ExportTraceServiceRequest {
	res := &resourcepb.Resource{}
	if service != "" {
		res.Attributes = []*commonpb.KeyValue{{Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: service}}}}
	}
	return &collectortrace.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{Resource: res, ScopeSpans: []*tracepb.ScopeSpans{{Spans: spans}}}}}
}

var (
	tid = []byte("0123456789abcdef")
	sid = []byte("01234567")
)

func TestConvertAcceptsValidSpan(t *testing.T) {
	res := Convert(request("checkout", span(tid, sid, now.Add(-time.Minute), now.Add(-time.Minute+50*time.Millisecond))), DefaultLimits(), now)
	if len(res.Spans) != 1 || res.RejectedCount() != 0 {
		t.Fatalf("got %+v", res)
	}
	s := res.Spans[0]
	if s.Service != "checkout" || s.TraceID != hex.EncodeToString(tid) || s.DurationNs != 50e6 {
		t.Errorf("unexpected span %+v", s)
	}
	if got := Convert(request("", span(tid, sid, now.Add(-time.Minute), now.Add(-time.Minute+1))), DefaultLimits(), now).Spans[0].Service; got != "unknown_service" {
		t.Errorf("missing service.name must become unknown_service, got %q", got)
	}
}

func TestConvertRejectsBadSpansByReason(t *testing.T) {
	ok := now.Add(-time.Minute)
	cases := []struct {
		name string
		sp   *tracepb.Span
		want string
	}{
		{"short trace id", span([]byte("short"), sid, ok, ok.Add(time.Millisecond)), RejectTraceID},
		{"all-zero trace id", span(make([]byte, 16), sid, ok, ok.Add(time.Millisecond)), RejectTraceID},
		{"bad span id", span(tid, []byte{1, 2}, ok, ok.Add(time.Millisecond)), RejectSpanID},
		{"end before start", span(tid, sid, ok, ok.Add(-time.Second)), RejectTimestamp},
		{"zero start", &tracepb.Span{TraceId: tid, SpanId: sid}, RejectTimestamp},
		{"span lasts days", span(tid, sid, ok.Add(-72*time.Hour), ok), RejectTimestamp},
		{"in the future", span(tid, sid, now.Add(time.Hour), now.Add(time.Hour+time.Second)), RejectFuture},
		{"too old", span(tid, sid, now.Add(-48*time.Hour), now.Add(-48*time.Hour+time.Second)), RejectTooOld},
	}
	for _, c := range cases {
		res := Convert(request("svc", c.sp), DefaultLimits(), now)
		if len(res.Spans) != 0 || res.Rejected[c.want] != 1 {
			t.Errorf("%s: want one %q rejection, got spans=%d rejected=%v", c.name, c.want, len(res.Spans), res.Rejected)
		}
	}
}

func TestBadSpansDoNotBlockGoodOnes(t *testing.T) {
	ok := now.Add(-time.Minute)
	res := Convert(request("svc", span([]byte("x"), sid, ok, ok.Add(1)), span(tid, sid, ok, ok.Add(1))), DefaultLimits(), now)
	if len(res.Spans) != 1 || res.RejectedCount() != 1 {
		t.Fatalf("got %d spans, %d rejected", len(res.Spans), res.RejectedCount())
	}
}

func TestLimitsTruncateOversizedFields(t *testing.T) {
	lim := DefaultLimits()
	sp := span(tid, sid, now.Add(-time.Minute), now.Add(-time.Minute+time.Millisecond))
	sp.Name = string(make([]byte, 5000))
	for i := 0; i < 200; i++ {
		sp.Attributes = append(sp.Attributes, &commonpb.KeyValue{Key: fmt.Sprintf("k%d", i), Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "v"}}})
	}
	got := Convert(request("svc", sp), lim, now).Spans[0]
	if len(got.Name) > lim.MaxNameLen+4 || len(got.Attributes) > lim.MaxAttributes {
		t.Errorf("limits not applied: name=%d attrs=%d", len(got.Name), len(got.Attributes))
	}
}

func TestUnmarshalProtobufAndJSONAgree(t *testing.T) {
	want := request("svc", span(tid, sid, now.Add(-time.Minute), now.Add(-time.Minute+time.Millisecond)))
	pb, _ := proto.Marshal(want)
	fromPB, err := Unmarshal(ContentProtobuf, pb)
	if err != nil {
		t.Fatal(err)
	}
	// OTLP/JSON carries IDs as hex text; the decoder must restore the real bytes.
	js := fmt.Sprintf(`{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"svc"}}]},"scopeSpans":[{"spans":[{"traceId":"%s","spanId":"%s","name":"GET /x","kind":2,"startTimeUnixNano":"%d","endTimeUnixNano":"%d","futureField":1}]}]}]}`,
		hex.EncodeToString(tid), hex.EncodeToString(sid), now.Add(-time.Minute).UnixNano(), now.Add(-time.Minute+time.Millisecond).UnixNano())
	fromJS, err := Unmarshal(ContentJSON, []byte(js))
	if err != nil {
		t.Fatal(err)
	}
	a, b := Convert(fromPB, DefaultLimits(), now), Convert(fromJS, DefaultLimits(), now)
	if len(a.Spans) != 1 || len(b.Spans) != 1 || a.Spans[0].TraceID != b.Spans[0].TraceID || a.Spans[0].SpanID != b.Spans[0].SpanID {
		t.Fatalf("protobuf and JSON decode differently:\n%+v\n%+v", a, b)
	}
}

func TestUnmarshalErrors(t *testing.T) {
	if _, err := Unmarshal("text/plain", []byte("x")); err != ErrUnsupportedContentType {
		t.Errorf("got %v", err)
	}
	if _, err := Unmarshal(ContentProtobuf, []byte{0xff, 0xff, 0xff}); err == nil {
		t.Error("garbage protobuf must fail")
	}
	if _, err := Unmarshal(ContentJSON, []byte("{not json")); err == nil {
		t.Error("garbage JSON must fail")
	}
}
