package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
)

func runningStatus(exitOnline bool) *ipnstate.Status {
	return &ipnstate.Status{
		BackendState: ipn.Running.String(),
		ExitNodeStatus: &ipnstate.ExitNodeStatus{
			ID:     "nodeid-1",
			Online: exitOnline,
		},
	}
}

// TestMonitorCheck covers the states the watchdog must distinguish: a healthy
// node, a disconnected backend, and the stale-exit-node case that does not heal
// on its own and so must trigger a rebind.
func TestMonitorCheck(t *testing.T) {
	tests := []struct {
		name         string
		status       *ipnstate.Status
		statusErr    error
		rebindErr    error
		wantHealthy  bool
		wantRebind   bool
		wantContains string
	}{
		{
			name:        "running with online exit node",
			status:      runningStatus(true),
			wantHealthy: true,
		},
		{
			name:         "status call fails",
			statusErr:    errors.New("boom"),
			wantContains: "Status: boom",
		},
		{
			name:         "backend not running",
			status:       &ipnstate.Status{BackendState: ipn.NeedsLogin.String()},
			wantContains: "backend state is \"NeedsLogin\"",
		},
		{
			name:         "exit node offline triggers rebind",
			status:       runningStatus(false),
			wantRebind:   true,
			wantContains: "rebound to",
		},
		{
			name:         "exit node missing triggers rebind",
			status:       &ipnstate.Status{BackendState: ipn.Running.String()},
			wantRebind:   true,
			wantContains: "exit node is not set",
		},
		{
			name:         "rebind failure is reported",
			status:       runningStatus(false),
			rebindErr:    errors.New("no peer"),
			wantRebind:   true,
			wantContains: "rebinding failed: no peer",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rebound := false
			m := &monitor{
				exitNode: "exit-1",
				interval: time.Second,
				status: func(context.Context) (*ipnstate.Status, error) {
					return tt.status, tt.statusErr
				},
				rebind: func(context.Context) error {
					rebound = true
					return tt.rebindErr
				},
			}

			reason := m.check(context.Background())

			if tt.wantHealthy {
				if reason != "" {
					t.Fatalf("check() = %q, want healthy (empty)", reason)
				}
			} else if reason == "" {
				t.Fatalf("check() = healthy, want a failure reason")
			}
			if tt.wantContains != "" && !strings.Contains(reason, tt.wantContains) {
				t.Errorf("check() = %q, want it to contain %q", reason, tt.wantContains)
			}
			if rebound != tt.wantRebind {
				t.Errorf("rebind called = %v, want %v", rebound, tt.wantRebind)
			}
		})
	}
}

// TestMonitorRunExitsAfterConsecutiveFailures verifies the self-termination
// path that lets `restart: unless-stopped` recycle a wedged container.
func TestMonitorRunExitsAfterConsecutiveFailures(t *testing.T) {
	m := &monitor{
		exitNode: "exit-1",
		interval: time.Millisecond,
		status: func(context.Context) (*ipnstate.Status, error) {
			return nil, errors.New("down")
		},
		rebind: func(context.Context) error { return nil },
	}
	m.setHealthy()

	failed := make(chan int, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go m.run(ctx, 3, failed)

	select {
	case n := <-failed:
		if n != 3 {
			t.Errorf("failure count = %d, want 3", n)
		}
	case <-ctx.Done():
		t.Fatal("run() did not report failure before the deadline")
	}
}

// TestMonitorRunRecovers verifies that a transient failure does not latch: once
// checks pass again the monitor reports healthy and resets the failure count.
func TestMonitorRunRecovers(t *testing.T) {
	var healthy atomic.Bool

	m := &monitor{
		exitNode: "exit-1",
		interval: time.Millisecond,
		status: func(context.Context) (*ipnstate.Status, error) {
			if healthy.Load() {
				return runningStatus(true), nil
			}
			return nil, errors.New("down")
		},
		rebind: func(context.Context) error { return nil },
	}
	m.setHealthy()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// exitAfter=0 means the loop never self-terminates.
	go m.run(ctx, 0, make(chan int, 1))

	// Wait for at least one failed check.
	deadline := time.After(5 * time.Second)
	for {
		if ok, _, _, n := m.snapshot(); !ok && n > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("monitor never went unhealthy")
		case <-time.After(time.Millisecond):
		}
	}

	healthy.Store(true)

	for {
		if ok, _, _, n := m.snapshot(); ok && n == 0 {
			return
		}
		select {
		case <-deadline:
			t.Fatal("monitor never recovered")
		case <-time.After(time.Millisecond):
		}
	}
}

// TestHealthHandler checks the status codes and payload Docker's HEALTHCHECK
// relies on.
func TestHealthHandler(t *testing.T) {
	m := &monitor{exitNode: "exit-1", interval: time.Second}
	m.setHealthy()

	srv := httptest.NewServer(m.healthHandler())
	defer srv.Close()

	get := func() (int, map[string]any) {
		resp, err := http.Get(srv.URL + "/healthz")
		if err != nil {
			t.Fatalf("GET /healthz: %v", err)
		}
		defer resp.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		return resp.StatusCode, body
	}

	code, body := get()
	if code != http.StatusOK {
		t.Errorf("healthy status = %d, want 200", code)
	}
	if body["status"] != "ok" {
		t.Errorf("status = %v, want ok", body["status"])
	}
	if body["exit_node"] != "exit-1" {
		t.Errorf("exit_node = %v, want exit-1", body["exit_node"])
	}

	m.setUnhealthy("exit node offline")

	code, body = get()
	if code != http.StatusServiceUnavailable {
		t.Errorf("unhealthy status = %d, want 503", code)
	}
	if body["status"] != "unhealthy" {
		t.Errorf("status = %v, want unhealthy", body["status"])
	}
	if body["reason"] != "exit node offline" {
		t.Errorf("reason = %v, want %q", body["reason"], "exit node offline")
	}
	if body["consecutive_failures"] != float64(1) {
		t.Errorf("consecutive_failures = %v, want 1", body["consecutive_failures"])
	}
}

func TestRemoveHopByHopHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Connection", "keep-alive")
	h.Set("Keep-Alive", "timeout=5")
	h.Set("Proxy-Authorization", "Basic xxx")
	h.Set("Transfer-Encoding", "chunked")
	h.Set("Content-Type", "text/plain")
	h.Set("X-Custom", "value")

	removeHopByHopHeaders(h)

	for _, hop := range hopByHopHeaders {
		if got := h.Get(hop); got != "" {
			t.Errorf("hop-by-hop header %q was not removed, got %q", hop, got)
		}
	}
	if got := h.Get("Content-Type"); got != "text/plain" {
		t.Errorf("Content-Type = %q, want text/plain", got)
	}
	if got := h.Get("X-Custom"); got != "value" {
		t.Errorf("X-Custom = %q, want value", got)
	}
}

func TestCopyHeaders(t *testing.T) {
	src := http.Header{}
	src.Add("X-Multi", "a")
	src.Add("X-Multi", "b")
	src.Set("X-Single", "one")

	dst := http.Header{}
	copyHeaders(dst, src)

	if got := dst.Values("X-Multi"); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("X-Multi = %v, want [a b]", got)
	}
	if got := dst.Get("X-Single"); got != "one" {
		t.Errorf("X-Single = %q, want one", got)
	}
}

// TestHandleHTTP exercises plain HTTP proxying end to end: a client configured
// to use the proxy reaches a backend through it, and hop-by-hop headers are
// stripped while end-to-end headers and the body pass through.
func TestHandleHTTP(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The proxy must strip this on the way in.
		if got := r.Header.Get("Proxy-Authorization"); got != "" {
			t.Errorf("backend received Proxy-Authorization = %q, want it stripped", got)
		}
		w.Header().Set("X-Backend", "hit")
		w.Header().Set("Connection", "close") // proxy must strip on the way out
		fmt.Fprint(w, "hello from backend")
	}))
	defer backend.Close()

	proxy := &forwardProxy{transport: http.DefaultTransport.(*http.Transport).Clone()}
	proxySrv := httptest.NewServer(proxy)
	defer proxySrv.Close()

	proxyURL, _ := url.Parse(proxySrv.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}

	req, _ := http.NewRequest(http.MethodGet, backend.URL, nil)
	req.Header.Set("Proxy-Authorization", "Basic secret")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request through proxy failed: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello from backend" {
		t.Errorf("body = %q, want %q", body, "hello from backend")
	}
	if got := resp.Header.Get("X-Backend"); got != "hit" {
		t.Errorf("X-Backend = %q, want hit", got)
	}
	if got := resp.Header.Get("Connection"); got != "" {
		t.Errorf("Connection header = %q, want it stripped", got)
	}
}

// TestHandleConnect exercises the CONNECT tunnel end to end against a TCP echo
// server, including bytes the client pipelines immediately after the CONNECT
// request line (the buffered-reader drain path).
func TestHandleConnect(t *testing.T) {
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen echo: %v", err)
	}
	defer echoLn.Close()
	go func() {
		for {
			c, err := echoLn.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()

	proxy := &forwardProxy{transport: http.DefaultTransport.(*http.Transport).Clone()}
	proxySrv := httptest.NewServer(proxy)
	defer proxySrv.Close()

	proxyAddr := proxySrv.Listener.Addr().String()
	echoAddr := echoLn.Addr().String()

	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()

	// Send the CONNECT request and pipeline the payload in the same write so
	// the proxy must drain the buffered bytes left over after the request.
	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\nping", echoAddr, echoAddr); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200", resp.StatusCode)
	}

	// The pipelined "ping" should come back echoed.
	buf := make([]byte, 4)
	if _, err := io.ReadFull(br, buf); err != nil {
		t.Fatalf("read echoed pipelined bytes: %v", err)
	}
	if string(buf) != "ping" {
		t.Errorf("pipelined echo = %q, want ping", buf)
	}

	// And the tunnel should keep working for subsequent writes.
	if _, err := conn.Write([]byte("pong")); err != nil {
		t.Fatalf("write to tunnel: %v", err)
	}
	if _, err := io.ReadFull(br, buf); err != nil {
		t.Fatalf("read echoed bytes: %v", err)
	}
	if string(buf) != "pong" {
		t.Errorf("tunnel echo = %q, want pong", buf)
	}
}
