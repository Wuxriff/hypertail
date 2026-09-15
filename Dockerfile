FROM golang:1.26 AS build

WORKDIR /app

COPY . .

RUN CGO_ENABLED=0 go build -o hypertail .

FROM alpine:latest

COPY --from=build /app/hypertail /usr/local/bin/hypertail

HEALTHCHECK --interval=30s --timeout=5s --start-period=90s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:8081/healthz || exit 1

ENTRYPOINT ["hypertail"]
