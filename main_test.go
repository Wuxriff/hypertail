package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

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
