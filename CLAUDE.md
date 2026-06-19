# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

`hypertail` is a single-binary local HTTP/HTTPS forward proxy that routes traffic
through a [Tailscale exit node](https://tailscale.com/kb/1103/exit-nodes). It
embeds a Tailscale node in-process via [`tsnet`](https://pkg.go.dev/tailscale.com/tsnet)
(no `tailscaled` daemon) and exposes a plain `HTTP`/`CONNECT` proxy on localhost,
so only tools explicitly pointed at the proxy egress through the exit node.

Requires Go 1.26+.

## Commands

```sh
go build -o hypertail .          # build the binary
go test -race ./...              # run the full suite (includes e2e HTTP + CONNECT tests)
go test -run TestHandleConnect . # run a single test
go vet ./...                     # vet

docker build -t hypertail .      # build container image
```

Run it (requires a `-exit-node`; first run prints a Tailscale auth URL to logs):

```sh
go run . -exit-node <node>
curl -x http://127.0.0.1:8080 https://example.com
```

## Architecture

The entire program lives in `main.go` (~240 lines); `main_test.go` holds the tests.
There are no internal packages. Two concerns make up the design:

**1. tsnet bring-up and exit-node selection** (`main`, `setExitNode`)
- `tsnet.Server` joins the tailnet, then `setExitNode` configures the embedded
  node's prefs to route egress through the chosen exit node.
- Key subtlety: the netmap/peer list is not populated immediately after the node
  comes up, so `setExitNode` **polls `lc.Status` until `SetExitNodeIP` resolves**
  the selector to a known peer (or the 60s context expires). Don't replace this
  with a single attempt — it will flake on cold start.

**2. The forward proxy** (`forwardProxy`)
- `ServeHTTP` dispatches `CONNECT` (HTTPS tunneling) vs. plain HTTP forwarding.
- All outbound dials go through `srv.Dial` (the tsnet node) via the custom
  `http.Transport.DialContext` — this is what forces traffic onto the exit
  node's route. Preserve this wiring when touching transport setup.
- `handleHTTP` strips hop-by-hop headers (per RFC 7230) both inbound and
  outbound and round-trips the request.
- `handleConnect` hijacks the client connection and bidirectionally copies. It
  copies from the buffered reader (`clientBuf`), not the raw conn, to drain
  bytes the client may have pipelined after the `CONNECT` line — the e2e test
  `TestHandleConnect` specifically exercises this path.

## Testing notes

The proxy logic (header handling, HTTP forwarding, CONNECT tunneling) is fully
unit/e2e tested using `httptest` and a local TCP echo server — no tailnet
needed. The `tsnet`/exit-node bring-up requires a live tailnet and is **not**
covered by tests, so changes there must be validated manually against a real
exit node.
