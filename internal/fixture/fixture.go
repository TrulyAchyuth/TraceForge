// Package fixture builds synthetic trace datasets for tests. The traces model
// the sample application's checkout flow with exact, known timings so that
// analysis and replay results can be asserted precisely.
package fixture

import (
	"fmt"
	"time"

	"github.com/TrulyAchyuth/TraceForge/internal/model"
)

const ms = int64(time.Millisecond)

// Builder accumulates dataset spans.
type Builder struct {
	Spans []model.DatasetSpan
	seq   int
}

// Checkout describes one checkout request.
type Checkout struct {
	// PaymentMs is the payment service's handling time for each attempt.
	// Attempts continue until one finishes inside TimeoutMs without error.
	PaymentMs []float64
	// PaymentErr marks attempts where payment itself returned an error.
	PaymentErr []bool
	// PaymentDown records the attempts with no payment span at all.
	PaymentDown bool
	// TimeoutMs is checkout's per-attempt client timeout (default 800).
	TimeoutMs float64
	// InventoryMs is inventory's handling time (default 10).
	InventoryMs float64
}

// Fixed timings of the modelled flow, in milliseconds.
const (
	GatewaySelfMs    = 1
	CheckoutSelfMs   = 2
	HopOverheadMs    = 1
	BackoffMs        = 50
	NotificationMs   = 5
	DefaultTimeoutMs = 800
	DefaultInventory = 10
)

func (b *Builder) id() string {
	b.seq++
	return fmt.Sprintf("%016x", b.seq)
}

func (b *Builder) add(phase, trace, parent, service, name string, kind int, start time.Time, durNs int64, err bool, mod func(*model.DatasetSpan)) string {
	sp := model.DatasetSpan{Phase: phase}
	sp.TraceID, sp.SpanID, sp.ParentSpanID = trace, b.id(), parent
	sp.Service, sp.Name, sp.Kind = service, name, kind
	sp.Start, sp.DurationNs = start.UTC(), durNs
	sp.StatusCode = model.StatusUnset
	if err {
		sp.StatusCode = model.StatusError
	}
	if mod != nil {
		mod(&sp)
	}
	b.Spans = append(b.Spans, sp)
	return sp.SpanID
}

// AddCheckout appends one checkout trace and returns its trace ID.
func (b *Builder) AddCheckout(phase string, start time.Time, c Checkout) string {
	b.seq++
	trace := fmt.Sprintf("%032x", b.seq)
	timeout := c.TimeoutMs
	if timeout == 0 {
		timeout = DefaultTimeoutMs
	}
	inv := c.InventoryMs
	if inv == 0 {
		inv = DefaultInventory
	}
	toNs := func(v float64) int64 { return int64(v * float64(ms)) }

	// Lay out checkout's work first to learn its duration.
	type call struct {
		offset, clientNs, serverNs int64
		clientErr, serverErr       bool
		timeout, noServer          bool
		resend                     int
	}
	cursor := CheckoutSelfMs * ms
	invCall := call{offset: cursor, serverNs: toNs(inv), clientNs: toNs(inv) + HopOverheadMs*ms}
	cursor += invCall.clientNs + ms
	var attempts []call
	failed := true
	for i, p := range c.PaymentMs {
		a := call{offset: cursor, serverNs: toNs(p), resend: i}
		natural := a.serverNs + HopOverheadMs*ms
		switch {
		case c.PaymentDown:
			a.noServer, a.clientErr, a.clientNs = true, true, 2*ms
		case natural >= toNs(timeout):
			a.timeout, a.clientErr, a.clientNs = true, true, toNs(timeout)
		default:
			a.clientNs = natural
			if i < len(c.PaymentErr) && c.PaymentErr[i] {
				a.serverErr, a.clientErr = true, true
			}
		}
		attempts = append(attempts, a)
		cursor += a.clientNs
		if !a.clientErr {
			failed = false
			break
		}
		if i < len(c.PaymentMs)-1 {
			cursor += BackoffMs * ms
		}
	}
	var notify *call
	if !failed {
		cursor += ms
		notify = &call{offset: cursor, serverNs: NotificationMs * ms, clientNs: (NotificationMs + HopOverheadMs) * ms}
		cursor += notify.clientNs
	}
	checkoutNs := cursor + ms
	gwClientNs := checkoutNs + HopOverheadMs*ms
	gatewayNs := GatewaySelfMs*ms + gwClientNs + ms

	root := b.add(phase, trace, "", "gateway", "GET /api/checkout", model.KindServer, start, gatewayNs, failed, nil)
	gwClientStart := start.Add(time.Duration(GatewaySelfMs * ms))
	gwClient := b.add(phase, trace, root, "gateway", "POST", model.KindClient, gwClientStart, gwClientNs, failed,
		func(s *model.DatasetSpan) { s.PeerService = "checkout" })
	coStart := gwClientStart.Add(time.Duration(HopOverheadMs * ms / 2))
	co := b.add(phase, trace, gwClient, "checkout", "POST /checkout", model.KindServer, coStart, checkoutNs, failed, nil)

	emit := func(c call, callee, serverName string) {
		at := coStart.Add(time.Duration(c.offset))
		client := b.add(phase, trace, co, "checkout", "POST", model.KindClient, at, c.clientNs, c.clientErr,
			func(s *model.DatasetSpan) {
				s.PeerService, s.ResendCount = callee, c.resend
				if c.timeout {
					s.ErrorType, s.StatusMessage = "timeout", "context deadline exceeded"
				}
			})
		if !c.noServer {
			b.add(phase, trace, client, callee, serverName, model.KindServer, at.Add(time.Duration(HopOverheadMs*ms/2)),
				c.serverNs, c.serverErr, nil)
		}
	}
	emit(invCall, "inventory", "POST /reserve")
	for _, a := range attempts {
		emit(a, "payment", "POST /charge")
	}
	if notify != nil {
		emit(*notify, "notification", "POST /notify")
	}
	return trace
}

// AddProducts appends one product-listing trace (gateway → inventory) and
// returns its trace ID. It does not touch checkout or payment.
func (b *Builder) AddProducts(phase string, start time.Time, inventoryMs float64) string {
	b.seq++
	trace := fmt.Sprintf("%032x", b.seq)
	invNs := int64(inventoryMs * float64(ms))
	clientNs := invNs + HopOverheadMs*ms
	root := b.add(phase, trace, "", "gateway", "GET /api/products", model.KindServer, start, GatewaySelfMs*ms+clientNs+ms, false, nil)
	clientStart := start.Add(time.Duration(GatewaySelfMs * ms))
	client := b.add(phase, trace, root, "gateway", "GET", model.KindClient, clientStart, clientNs, false,
		func(s *model.DatasetSpan) { s.PeerService = "inventory" })
	b.add(phase, trace, client, "inventory", "GET /stock", model.KindServer, clientStart.Add(time.Duration(HopOverheadMs*ms/2)), invNs, false, nil)
	return trace
}

// PaymentSlowdown returns a dataset modelling the reference incident: payment
// slows from ~40 ms to well beyond checkout's 800 ms timeout for most
// attempts, causing retries and checkout failures, while product listing is
// unaffected. Timings are fixed, so results are exactly reproducible.
func PaymentSlowdown() *model.Dataset {
	b := &Builder{}
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 40; i++ {
		at := t0.Add(time.Duration(i) * 2 * time.Second)
		b.AddCheckout(model.PhaseBaseline, at, Checkout{PaymentMs: []float64{38 + float64(i%5)}})
		b.AddProducts(model.PhaseBaseline, at.Add(500*time.Millisecond), 10+float64(i%3))
	}
	inc := t0.Add(10 * time.Minute)
	patterns := [][]float64{
		{950, 1100, 1000}, // three timeouts: fails
		{900, 600},        // one timeout, then success
		{700},             // slow success
		{1200, 950, 650},  // two timeouts, then success
		{1000, 1050, 990}, // fails
		{760},             // slow success
	}
	for i := 0; i < 60; i++ {
		at := inc.Add(time.Duration(i) * 2 * time.Second)
		b.AddCheckout(model.PhaseIncident, at, Checkout{PaymentMs: patterns[i%len(patterns)]})
		b.AddProducts(model.PhaseIncident, at.Add(500*time.Millisecond), 10+float64(i%3))
	}
	return &model.Dataset{
		ID: 1, IncidentID: 1, Version: 1, BaselineStart: t0, IncidentStart: inc, WindowEnd: inc.Add(2 * time.Minute),
		Spans: b.Spans,
		Meta: model.DatasetMeta{BaselineTracesAvailable: 80, BaselineTracesSampled: 80,
			IncidentTracesAvailable: 120, IncidentTracesSampled: 120, BucketSeconds: 10},
	}
}
