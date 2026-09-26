# syntax=docker/dockerfile:1

# ---- build stage ----
FROM golang:1.26-alpine AS build

WORKDIR /src

# Cache module downloads separately from the source.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Static binary so it runs in a minimal final image.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/hypertail .

# ---- runtime stage ----
FROM alpine:3.20

# CA certificates for verifying upstream TLS targets.
RUN apk add --no-cache ca-certificates

COPY --from=build /out/hypertail /usr/local/bin/hypertail

# tsnet state is persisted here; mount a volume to keep it across restarts.
RUN mkdir -p /var/lib/hypertail
VOLUME /var/lib/hypertail

# Proxy port (bound to 0.0.0.0 below so it is reachable from the host).
EXPOSE 8080

# Stable defaults baked in; pass -exit-node (and override others if needed) at
# `docker run`. Go's flag parser lets a later value win, so e.g. an extra
# -listen on the command line overrides the default here.
ENTRYPOINT ["hypertail", "-listen", "0.0.0.0:8080", "-state-dir", "/var/lib/hypertail"]
