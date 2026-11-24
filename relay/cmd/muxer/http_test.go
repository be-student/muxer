package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/gantoine/muxer/relay/internal/devices"
	"github.com/gantoine/muxer/relay/internal/server"
)

func startHTTPRelay(t *testing.T, readTimeout time.Duration) (*httptest.Server, *devices.Store) {
	t.Helper()
	store, err := devices.Open(filepath.Join(t.TempDir(), "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	// These routes do not call cmux: pairing, health, and an unknown WS request.
	relay := server.New(nil, store, nil, server.Options{MacName: "Test Mac"}, slog.New(slog.DiscardHandler))
	host := httptest.NewUnstartedServer(relay.Handler())
	host.Config = newHTTPServer("", relay.Handler())
	if host.Config.ReadTimeout != 10*time.Second || host.Config.ReadHeaderTimeout != 10*time.Second {
		t.Fatal("the production server must bound headers and complete requests to 10 seconds")
	}
	if readTimeout != 0 {
		host.Config.ReadTimeout = readTimeout
	}
	host.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		relay.Close(ctx)
		host.Close()
	})
	return host, store
}

func TestHTTPReadTimeoutBoundsPairingBodyAndUnreadTail(t *testing.T) {
	for _, completeJSON := range []bool{false, true} {
		name := "partial JSON"
		if completeJSON {
			name = "complete JSON with stalled tail"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			host, store := startHTTPRelay(t, 100*time.Millisecond)
			code, _, err := store.NewPairingCode()
			if err != nil {
				t.Fatal(err)
			}
			body := fmt.Sprintf(`{"code":%q,"device_name":"test"}`, code)
			prefix := body[:9]
			wantStatus := http.StatusBadRequest
			if completeJSON {
				prefix = body
				// Preserve the existing decoder behavior; still bound net/http's
				// drain of bytes following a complete JSON value.
				wantStatus = http.StatusOK
			}
			conn, err := net.DialTimeout("tcp", host.Listener.Addr().String(), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := fmt.Fprintf(conn, "POST /v1/pair HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body)+1, prefix); err != nil {
				t.Fatal(err)
			}
			response, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatalf("the read timeout must finish the response: %v", err)
			}
			defer response.Body.Close()
			if _, err := io.Copy(io.Discard, response.Body); err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != wantStatus || !response.Close {
				t.Fatalf("stalled body: status=%d close=%t, want %d and a closed connection", response.StatusCode, response.Close, wantStatus)
			}
			if _, err := conn.Read(make([]byte, 1)); err != io.EOF {
				t.Fatalf("the stalled connection must be closed, got %v", err)
			}
		})
	}
}

func TestPairingKeepAliveAndWebSocketOutliveHTTPReadTimeout(t *testing.T) {
	t.Parallel()
	host, store := startHTTPRelay(t, 0)
	code, _, err := store.NewPairingCode()
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{MaxIdleConnsPerHost: 1, MaxConnsPerHost: 1}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	connections := make(chan net.Conn, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		connections <- info.Conn
	}})
	body, _ := json.Marshal(map[string]string{"code": code, "device_name": "test"})
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, host.URL+"/v1/pair", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	pairedJSON, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("pair: status=%d err=%v", response.StatusCode, err)
	}
	var paired struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(pairedJSON, &paired); err != nil || paired.Token == "" {
		t.Fatalf("pair token: %v", err)
	}
	firstConn := <-connections

	request, _ = http.NewRequestWithContext(ctx, http.MethodGet, host.URL+"/v1/health", nil)
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, readErr := io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusOK || <-connections != firstConn {
		t.Fatal("a successful pairing must allow another request on the same connection")
	}

	conn, _, err := websocket.Dial(ctx, host.URL+"/v1/ws", &websocket.DialOptions{
		HTTPClient: client,
		HTTPHeader: http.Header{"Authorization": {"Bearer " + paired.Token}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	// Use the real production timeout, not the shortened timeout in the body tests.
	time.Sleep(host.Config.ReadTimeout + 100*time.Millisecond)
	if err := wsjson.Write(ctx, conn, map[string]string{"id": "probe", "type": "unknown_test_request"}); err != nil {
		t.Fatal(err)
	}
	var result struct {
		ID    string `json:"id"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := wsjson.Read(ctx, conn, &result); err != nil {
		t.Fatalf("the HTTP deadline must not terminate an upgraded WebSocket: %v", err)
	}
	if result.ID != "probe" || result.Error.Code != "unknown_type" {
		t.Fatalf("unexpected WebSocket response: %+v", result)
	}
}
