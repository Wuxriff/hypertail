package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tsnet"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8080", "Local address to listen on for proxy requests")
	exitNode := flag.String("exit-node", "", "Tailscale exit node hostname or IP")
	hostname := flag.String("hostname", "hypertail", "Tailscale hostname for this node")
	stateDir := flag.String("state-dir", "", "Directory to store Tailscale state")
	verbose := flag.Bool("verbose", false, "Enable verbose logging")
	healthListen := flag.String("health-listen", "", "Address to serve the /healthz endpoint on (empty disables it)")
	healthInterval := flag.Duration("health-interval", 15*time.Second, "How often to check tsnet connectivity and exit node binding")
	unhealthyExitAfter := flag.Int("unhealthy-exit-after", 0, "Exit with a non-zero status after this many consecutive failed checks, so a container supervisor can restart the process (0 disables)")
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

	// The watchdog owns the liveness state that both /healthz and the
	// self-termination path read.
	mon := &monitor{
		status:   lc.Status,
		rebind:   func(ctx context.Context) error { return setExitNode(ctx, lc, *exitNode) },
		exitNode: *exitNode,
		interval: *healthInterval,
	}
	mon.setHealthy()

	runCtx, stopRun := context.WithCancel(ctx)
	defer stopRun()

	failed := make(chan int, 1)
	go mon.run(runCtx, *unhealthyExitAfter, failed)

	var healthSrv *http.Server
	if *healthListen != "" {
		healthSrv = &http.Server{
			Addr:    *healthListen,
			Handler: mon.healthHandler(),
		}
		go func() {
			log.Printf("Health endpoint listening on %s/healthz", *healthListen)
			if err := healthSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatalf("health server error: %v", err)
			}
		}()
	}

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

	exitCode := 0
	select {
	case <-sigCh:
		log.Println("Shutting down...")
	case n := <-failed:
		log.Printf("Exiting after %d consecutive failed health checks so the supervisor can restart us", n)
		exitCode = 1
	}

	stopRun()
	shutdownCtx, stopShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopShutdown()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP server shutdown: %v", err)
	}
	if healthSrv != nil {
		if err := healthSrv.Shutdown(shutdownCtx); err != nil {
			log.Printf("health server shutdown: %v", err)
		}
	}

	if exitCode != 0 {
		// srv.Close is deferred; run it before bypassing the deferred chain.
		srv.Close()
		os.Exit(exitCode)
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

// monitor periodically verifies that the tsnet node is still connected to the
// control plane and that the configured exit node is still bound and online.
//
// tsnet reconnects to the control plane on its own, but the exit node binding
// does not heal itself: setExitNode resolves the selector to a concrete node ID
// once at startup, so if the exit node leaves the netmap and comes back with a
// new ID, prefs keep pointing at a peer that no longer exists and egress stays
// broken indefinitely. The watchdog re-resolves the selector when that happens.
type monitor struct {
	// status and rebind are injected so the check loop can be tested without a
	// live tailnet; in production they wrap the tsnet local client.
	status   func(context.Context) (*ipnstate.Status, error)
	rebind   func(context.Context) error
	exitNode string
	interval time.Duration

	mu       sync.Mutex
	healthy  bool
	reason   string
	since    time.Time
	failures int
}

func (m *monitor) setHealthy() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.healthy {
		m.since = time.Now()
	}
	m.healthy = true
	m.reason = ""
	m.failures = 0
}

// setUnhealthy records a failed check and returns the number of consecutive
// failures so far.
func (m *monitor) setUnhealthy(reason string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.healthy {
		m.since = time.Now()
	}
	m.healthy = false
	m.reason = reason
	m.failures++
	return m.failures
}

func (m *monitor) snapshot() (healthy bool, reason string, since time.Time, failures int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.healthy, m.reason, m.since, m.failures
}

// check reports why the node is unhealthy, or "" when everything is in order.
// It attempts to repair a stale exit node binding before giving up.
func (m *monitor) check(ctx context.Context) string {
	st, err := m.status(ctx)
	if err != nil {
		return fmt.Sprintf("Status: %v", err)
	}

	if st.BackendState != ipn.Running.String() {
		return fmt.Sprintf("backend state is %q, want %q", st.BackendState, ipn.Running.String())
	}

	if st.ExitNodeStatus != nil && st.ExitNodeStatus.Online {
		return ""
	}

	// Either prefs lost the exit node entirely or the bound peer went offline.
	// Re-resolving the selector picks up a node that rejoined under a new ID.
	what := "exit node is not set"
	if st.ExitNodeStatus != nil {
		what = fmt.Sprintf("exit node %s is offline", st.ExitNodeStatus.ID)
	}
	log.Printf("Health check: %s, re-resolving %q", what, m.exitNode)

	retryCtx, cancel := context.WithTimeout(ctx, m.interval)
	defer cancel()
	if err := m.rebind(retryCtx); err != nil {
		return fmt.Sprintf("%s, and rebinding failed: %v", what, err)
	}

	// EditPrefs has been accepted, but the binding only counts as recovered
	// once the node reports the exit node online again. Report the current
	// state as unhealthy and let the next tick confirm the repair.
	return fmt.Sprintf("%s, rebound to %q, waiting for it to come online", what, m.exitNode)
}

// run drives the check loop. When exitAfter is greater than zero and that many
// checks fail consecutively, the count is sent on failed so main can exit and
// let the container supervisor restart the process.
func (m *monitor) run(ctx context.Context, exitAfter int, failed chan<- int) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		if reason := m.check(ctx); reason != "" {
			n := m.setUnhealthy(reason)
			log.Printf("Health check failed (%d consecutive): %s", n, reason)
			if exitAfter > 0 && n >= exitAfter {
				select {
				case failed <- n:
				default:
				}
				return
			}
			continue
		}

		if healthy, _, _, _ := m.snapshot(); !healthy {
			log.Printf("Health check recovered")
		}
		m.setHealthy()
	}
}

func (m *monitor) healthHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		healthy, reason, since, failures := m.snapshot()

		body := struct {
			Status            string `json:"status"`
			ExitNode          string `json:"exit_node"`
			Reason            string `json:"reason,omitempty"`
			SinceRFC3339      string `json:"since"`
			ConsecutiveErrors int    `json:"consecutive_failures"`
		}{
			Status:            "ok",
			ExitNode:          m.exitNode,
			Reason:            reason,
			SinceRFC3339:      since.UTC().Format(time.RFC3339),
			ConsecutiveErrors: failures,
		}

		code := http.StatusOK
		if !healthy {
			body.Status = "unhealthy"
			code = http.StatusServiceUnavailable
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(body)
	})
	return mux
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
