// Command loadtest drives sustained OTLP ingestion and concurrent API reads against a running
// TraceForge, and prints a JSON summary. It exists for scripts/reliability.sh; it is not part of the product.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

type stats struct {
	mu    sync.Mutex
	lat   []float64
	codes map[string]int
}

func (s *stats) add(ms float64, code string) {
	s.mu.Lock()
	s.lat = append(s.lat, ms)
	s.codes[code]++
	s.mu.Unlock()
}

func (s *stats) summary() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := append([]float64(nil), s.lat...)
	sort.Float64s(l)
	q := func(p float64) float64 {
		if len(l) == 0 {
			return 0
		}
		return l[min(int(p*float64(len(l))), len(l)-1)]
	}
	return map[string]any{"requests": len(l), "p50_ms": q(.5), "p95_ms": q(.95), "p99_ms": q(.99), "max_ms": q(1), "status": s.codes}
}

func main() {
	ingest := flag.String("ingest", "http://localhost:4318/v1/traces", "OTLP/HTTP endpoint")
	api := flag.String("api", "http://localhost:8080", "API base URL for read probes")
	token := flag.String("token", "", "API bearer token")
	rate := flag.Int("rate", 500, "target spans per second")
	batch := flag.Int("batch", 50, "spans per export request")
	dur := flag.Duration("duration", 5*time.Minute, "test duration")
	service := flag.String("service", "loadtest", "service.name used for generated spans")
	flag.Parse()

	client := &http.Client{Timeout: 10 * time.Second}
	ing, probe := &stats{codes: map[string]int{}}, &stats{codes: map[string]int{}}
	var sentSpans, ackedSpans, retries, gaveUp atomic.Int64
	prefix := rand.Uint64()
	var seq atomic.Uint64
	end := time.Now().Add(*dur)

	build := func() []byte {
		now := time.Now()
		spans := make([]*tracepb.Span, *batch)
		for i := range spans {
			n := seq.Add(1)
			tid, sid := make([]byte, 16), make([]byte, 8)
			for j := 0; j < 8; j++ {
				tid[j], tid[8+j], sid[j] = byte(prefix>>(8*j)), byte(n>>(8*j)), byte(n>>(8*j))|1
			}
			d := time.Duration(5+rand.Intn(45)) * time.Millisecond
			spans[i] = &tracepb.Span{TraceId: tid, SpanId: sid, Name: "GET /loadtest", Kind: tracepb.Span_SPAN_KIND_SERVER,
				StartTimeUnixNano: uint64(now.Add(-d).UnixNano()), EndTimeUnixNano: uint64(now.UnixNano())}
		}
		b, _ := proto.Marshal(&collectortrace.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{
			Resource:   &resourcepb.Resource{Attributes: []*commonpb.KeyValue{{Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: *service}}}}},
			ScopeSpans: []*tracepb.ScopeSpans{{Spans: spans}}}}})
		return b
	}

	// send behaves like an OTLP exporter: it retries transient failures with backoff and gives up after 8 tries.
	send := func(body []byte) {
		sentSpans.Add(int64(*batch))
		backoff := 100 * time.Millisecond
		for try := 0; try < 8; try++ {
			t0 := time.Now()
			resp, err := client.Post(*ingest, "application/x-protobuf", bytes.NewReader(body))
			ms := float64(time.Since(t0).Microseconds()) / 1000
			if err != nil {
				ing.add(ms, "conn_error")
			} else {
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				ing.add(ms, fmt.Sprint(resp.StatusCode))
				if resp.StatusCode == 200 {
					ackedSpans.Add(int64(*batch))
					return
				}
				if resp.StatusCode != 429 && resp.StatusCode < 500 {
					break
				}
			}
			retries.Add(1)
			time.Sleep(backoff)
			backoff = min(backoff*2, 5*time.Second)
		}
		gaveUp.Add(int64(*batch))
	}

	var wg sync.WaitGroup
	interval := time.Duration(float64(time.Second) * float64(*batch) / float64(*rate))
	tick := time.NewTicker(interval)
	defer tick.Stop()
	sem := make(chan struct{}, 32)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for time.Now().Before(end) {
			<-tick.C
			select {
			case sem <- struct{}{}:
				wg.Add(1)
				go func() { defer wg.Done(); defer func() { <-sem }(); send(build()) }()
			default: // the exporter pool is saturated; count the shortfall as not attempted
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		paths := []string{"/api/v1/overview", "/api/v1/services", "/api/v1/incidents", "/api/v1/dependencies"}
		for i := 0; time.Now().Before(end); i++ {
			req, _ := http.NewRequest("GET", *api+paths[i%len(paths)], nil)
			if *token != "" {
				req.Header.Set("Authorization", "Bearer "+*token)
			}
			t0 := time.Now()
			resp, err := client.Do(req)
			ms := float64(time.Since(t0).Microseconds()) / 1000
			if err != nil {
				probe.add(ms, "conn_error")
			} else {
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				probe.add(ms, fmt.Sprint(resp.StatusCode))
			}
			time.Sleep(time.Second)
		}
	}()
	wg.Wait()

	out := map[string]any{"duration_s": dur.Seconds(), "target_spans_per_s": *rate, "batch": *batch,
		"spans_attempted": sentSpans.Load(), "spans_acked": ackedSpans.Load(), "spans_gave_up": gaveUp.Load(), "retried_requests": retries.Load(),
		"ingest": ing.summary(), "api_probe": probe.summary(), "unique_span_prefix_service": *service}
	_ = json.NewEncoder(os.Stdout).Encode(out)
}
