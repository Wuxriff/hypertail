# hypertail

A lightweight local HTTP/HTTPS forward proxy that routes your traffic through a
[Tailscale exit node](https://tailscale.com/kb/1103/exit-nodes) — without
touching your machine's system-wide network settings.

`hypertail` runs an in-process Tailscale node using
[`tsnet`](https://pkg.go.dev/tailscale.com/tsnet), selects an exit node, and
exposes a plain `HTTP`/`CONNECT` proxy on `localhost`. Point any tool that
understands a proxy (curl, browsers, `HTTP(S)_PROXY`, etc.) at it and that tool's
traffic — and only that tool's traffic — egresses through your chosen exit node.

## Features

- 🔌 **No system changes** — only apps you explicitly point at the proxy are affected.
- 🌍 **Exit node routing** — egress through any node in your tailnet by hostname or IP.
- 🔒 **HTTP and HTTPS** — full `CONNECT` tunneling for TLS, plus plain HTTP forwarding.
- 🧹 **Correct proxying** — strips hop-by-hop headers per RFC 7230.
- 🪶 **Single static binary** — just `tsnet`, no `tailscaled` daemon required.

## Installation

```sh
go install github.com/werat/hypertail@latest
```

Or build from source:

```sh
git clone https://github.com/werat/hypertail
cd hypertail
go build -o hypertail .
```

Requires Go 1.26+.

## Usage

```sh
hypertail -exit-node <node>
```

On first run, `tsnet` prints an authentication URL to the logs — open it to add
this node to your tailnet. State is persisted, so subsequent runs reconnect
automatically.

### Flags

| Flag          | Default            | Description                                          |
| ------------- | ------------------ | ---------------------------------------------------- |
| `-exit-node`  | _(required)_       | Tailscale exit node hostname or IP.                  |
| `-listen`     | `127.0.0.1:8080`   | Local address to listen on for proxy requests.       |
| `-hostname`   | `hypertail`        | Tailscale hostname for this node.                    |
| `-state-dir`  | _(tsnet default)_  | Directory to store Tailscale state.                  |
| `-verbose`    | `false`            | Enable verbose `tsnet` logging.                      |

## Examples

Start the proxy, routing through an exit node named `us-server`:

```sh
hypertail -exit-node us-server
```

Send a request through it with curl (works for both HTTP and HTTPS):

```sh
curl -x http://127.0.0.1:8080 https://example.com
```

Check the egress IP — it should be your exit node's:

```sh
curl -x http://127.0.0.1:8080 https://api.ipify.org
```

Route through an exit node by IP, on a custom port, with a persistent state
directory:

```sh
hypertail \
  -exit-node 100.101.102.103 \
  -listen 127.0.0.1:9090 \
  -state-dir ~/.config/hypertail
```

Export proxy environment variables so other tools pick it up automatically:

```sh
export HTTP_PROXY=http://127.0.0.1:8080
export HTTPS_PROXY=http://127.0.0.1:8080

curl https://example.com   # goes through the exit node
git clone https://...      # so does this
```

## How it works

```
                         ┌────────────────────── hypertail ───────────────────────┐
  curl / browser         │                                                         │      exit node
  ───────────────►  127.0.0.1:8080 (HTTP/CONNECT)  ──►  tsnet node  ──► tailnet ──►│──► the internet
  (HTTP_PROXY)           │                                                         │
                         └─────────────────────────────────────────────────────────┘
```

1. `tsnet` brings up an embedded Tailscale node and joins your tailnet.
2. `hypertail` sets that node's preferences to use the requested exit node.
3. The local proxy accepts requests and dials targets **through the tsnet
   node**, so all egress takes the exit node's route.

## Docker

The image is built and published to GHCR by Actions (multi-arch: amd64 +
arm64). Its version tracks the bundled `tailscale.com` module: Dependabot
opens a PR bumping go.mod, and merging it triggers a new build. The deployed
container uses the `latest` tag, so Watchtower (or a manual `docker compose
pull && docker compose up -d`) keeps it current.

### Run

`compose.yaml` in this repo is the deployment used on the Raspberry Pi (exit
node, state volume, health endpoint included):

```sh
docker compose up -d
```

Or without compose (the image binds the proxy to `0.0.0.0:8080` and stores
tsnet state in `/var/lib/hypertail`, so you only need to supply `-exit-node`):

```sh
docker run --rm -it \
  -p 127.0.0.1:8080:8080 \
  -v hypertail-state:/var/lib/hypertail \
  ghcr.io/wuxriff/hypertail:latest -exit-node us-server
```

> **First run — authentication.** `tsnet` prints a Tailscale login URL to the
> container logs. Watch for it and open it to add the node to your tailnet:
>
> ```sh
> docker logs -f <container>
> ```
>
> Thanks to the mounted volume, subsequent runs reconnect automatically.

Then point your tools at the published port (note the `http://` scheme — the
proxy itself speaks plain HTTP and tunnels HTTPS via `CONNECT`):

```sh
curl -x http://127.0.0.1:8080 https://api.ipify.org
```

Any flag can be overridden on the command line (a later value wins), e.g. to
change the listen port inside the container.

### Health

Run with `-health-listen=0.0.0.0:8081` (as `compose.yaml` does) and the
container serves a passive status endpoint:

```sh
curl http://127.0.0.1:8081/healthz
```

It reports the tsnet backend state and whether the exit node is bound and
online, answering 200 or 503 with a reason. It only observes: reconnection and
re-auth are tsnet's own job. The Dockerfile's `HEALTHCHECK` uses it to show
`(healthy)`/`(unhealthy)` in `docker ps`; nothing is restarted automatically.

> **Networking note.** `tsnet` uses userspace WireGuard, so no `--cap-add` or
> `/dev/net/tun` is required — only outbound network access for Tailscale to
> connect.

## Development

Run the test suite (includes end-to-end HTTP and CONNECT proxy tests):

```sh
go test -race ./...
```

The proxy logic (`forwardProxy`, header handling, CONNECT tunneling) is covered
by tests. The `tsnet`/exit-node bring-up requires a live tailnet and is not unit
tested.

## License

MIT
