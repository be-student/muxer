package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/gantoine/muxer/relay/internal/devices"
	"github.com/gantoine/muxer/relay/internal/host"
)

func TestRequestBudgetBackpressureAndCancellation(t *testing.T) {
	s := newSession(nil, nil, devices.Device{})
	s.ctx, s.cancel = context.WithCancel(context.Background())
	defer s.cancel()
	if !s.reserveMessage(maxPendingBytes) {
		t.Fatal("full request budget must be usable")
	}
	reserved := make(chan bool, 1)
	go func() { reserved <- s.reserveMessage(1) }()
	select {
	case <-reserved:
		t.Fatal("additional data bypassed the request byte budget")
	case <-time.After(20 * time.Millisecond):
	}
	s.releaseMessage(maxPendingBytes)
	select {
	case ok := <-reserved:
		if !ok {
			t.Fatal("backpressure did not resume after release")
		}
	case <-time.After(time.Second):
		t.Fatal("released bytes did not wake the reader")
	}
	s.releaseMessage(1)
	if got := s.pendingBytes.Load(); got != 0 {
		t.Fatalf("retained %d bytes", got)
	}
	if !s.reserveMessage(maxPendingBytes) {
		t.Fatal("budget could not be reused")
	}
	go func() { reserved <- s.reserveMessage(1) }()
	s.cancel()
	select {
	case ok := <-reserved:
		if ok {
			t.Fatal("canceled reader reserved bytes")
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not wake the reader")
	}
	s.releaseMessage(maxPendingBytes)
	if got := s.pendingBytes.Load(); got != 0 {
		t.Fatalf("cancellation retained %d bytes", got)
	}
}

func TestRequestBudgetIncludesExecutingRequestsAndDrains(t *testing.T) {
	started := make(chan struct{})
	relay := startRelayWith(t, func(options *Options) {
		options.HostUsage = func(ctx context.Context) (host.Usage, error) {
			close(started)
			<-ctx.Done()
			return host.Usage{}, ctx.Err()
		}
	})
	accepted := make(chan *session, 1)
	finished := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		conn.SetReadLimit(maxMessageBytes)
		s := newSession(relay.server, conn, devices.Device{})
		accepted <- s
		s.run()
		close(finished)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, strings.Replace(server.URL, "http", "ws", 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	s := <-accepted
	// A synchronous success and an invalid envelope both release their budget.
	for _, raw := range []string{`{"type":"hello","id":"sync"}`, `[]`} {
		if err := conn.Write(ctx, websocket.MessageText, []byte(raw)); err != nil {
			t.Fatal(err)
		}
		readUntil(t, ctx, conn, "result")
	}
	request := `{"type":"host_usage","id":"slow"}`
	if err := conn.Write(ctx, websocket.MessageText, []byte(request)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("parallel request did not start")
	}
	if got := s.pendingBytes.Load(); got < int64(len(request)) {
		t.Fatalf("executing request released its %d-byte body too soon: %d", len(request), got)
	}
	// Queue more messages and cancel while a parallel handler is still active.
	for i := 0; i < maxQueuedMessages; i++ {
		if err := wsjson.Write(ctx, conn, map[string]any{"type": "input", "text": "x"}); err != nil {
			t.Fatal(err)
		}
	}
	s.cancel()
	select {
	case <-finished:
	case <-ctx.Done():
		t.Fatal("session did not drain after cancellation")
	}
	if got := s.pendingBytes.Load(); got != 0 {
		t.Fatalf("completed/canceled handlers or queued messages retained %d bytes", got)
	}
}
