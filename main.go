package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/ipn"
	"tailscale.com/tsnet"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8080", "Local address to listen on for proxy requests")
	exitNode := flag.String("exit-node", "", "Tailscale exit node hostname or IP")
	hostname := flag.String("hostname", "hypertail", "Tailscale hostname for this node")
	stateDir := flag.String("state-dir", "", "Directory to store Tailscale state")
	verbose := flag.Bool("verbose", false, "Enable verbose logging")
	flag.Parse()

	if *exitNode == "" {
		fmt.Fprintf(os.Stderr, "Usage: hypertail -exit-node <node>\n\n")
		fmt.Fprintf(os.Stderr, "Run a local HTTP proxy that routes traffic through a Tailscale exit node.\n\n")
		fmt.Fprintf(os.Stderr, "Example:\n")
		fmt.Fprintf(os.Stderr, "  hypertail -exit-node us-server\n")
		fmt.Fprintf(os.Stderr, "  curl -x http://127.0.0.1:8080 https://example.com\n\n")
		flag.PrintDefaults()
		os.Exit(1)
	}

	srv := &tsnet.Server{
		Hostname: *hostname,
	}
	if *stateDir != "" {
		srv.Dir = *stateDir
	}
	if !*verbose {
		srv.Logf = func(string, ...any) {}
	}
	defer srv.Close()

	log.Printf("Starting tsnet node %q, exit node %q", *hostname, *exitNode)

	ctx := context.Background()
	if _, err := srv.Up(ctx); err != nil {
		log.Fatalf("tsnet failed to start: %v", err)
	}

	lc, err := srv.LocalClient()
	if err != nil {
		log.Fatalf("failed to get local client: %v", err)
	}

	setCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := setExitNode(setCtx, lc, *exitNode); err != nil {
		log.Fatalf("failed to set exit node: %v", err)
	}
	log.Printf("Exit node set to %q", *exitNode)

	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return srv.Dial(ctx, network, addr)
		},
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	proxy := &forwardProxy{transport: transport}

	httpSrv := &http.Server{
		Addr:    *listen,
		Handler: proxy,
	}

	go func() {
		log.Printf("Proxy listening on %s", *listen)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	log.Println("Shutting down...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP server shutdown: %v", err)
	}
}

func setExitNode(ctx context.Context, lc *local.Client, exitNodeSel string) error {
	prefs, err := lc.GetPrefs(ctx)
	if err != nil {
		return fmt.Errorf("GetPrefs: %w", err)
	}

	// The netmap (and thus the peer list) may not be populated immediately
	// after the node comes up, so poll Status until the exit node selector
	// resolves to a known peer or the context expires.
	np := prefs.Clone()
	np.ClearExitNode()

	var lastErr error
	for {
		st, err := lc.Status(ctx)
		if err != nil {
			lastErr = fmt.Errorf("Status: %w", err)
		} else if err := np.SetExitNodeIP(exitNodeSel, st); err != nil {
			lastErr = fmt.Errorf("SetExitNodeIP(%q): %w", exitNodeSel, err)
		} else {
			lastErr = nil
			break
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for exit node %q: %w (last error: %v)", exitNodeSel, ctx.Err(), lastErr)
		case <-time.After(500 * time.Millisecond):
		}
	}

	_, err = lc.EditPrefs(ctx, &ipn.MaskedPrefs{
		Prefs:         *np,
		ExitNodeIPSet: true,
		ExitNodeIDSet: true,
	})
	if err != nil {
		return fmt.Errorf("EditPrefs: %w", err)
	}

	return nil
}

type forwardProxy struct {
	transport *http.Transport
}

func (p *forwardProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	p.handleHTTP(w, r)
}

func (p *forwardProxy) handleHTTP(w http.ResponseWriter, r *http.Request) {
	r.RequestURI = ""
	removeHopByHopHeaders(r.Header)

	resp, err := p.transport.RoundTrip(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	removeHopByHopHeaders(resp.Header)
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func (p *forwardProxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	targetConn, err := p.transport.DialContext(r.Context(), "tcp", r.Host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer targetConn.Close()

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}

	clientConn, clientBuf, err := hj.Hijack()
	if err != nil {
		return
	}
	defer clientConn.Close()

	if _, err := clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}

	done := make(chan struct{}, 2)
	// Copy from clientBuf (not clientConn) so any bytes the client pipelined
	// after the CONNECT request are drained from the buffer before reading
	// directly from the socket.
	go func() {
		io.Copy(targetConn, clientBuf)
		done <- struct{}{}
	}()
	go func() {
		io.Copy(clientConn, targetConn)
		done <- struct{}{}
	}()
	<-done
}

func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

var hopByHopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

func removeHopByHopHeaders(h http.Header) {
	for _, hdr := range hopByHopHeaders {
		h.Del(hdr)
	}
}
