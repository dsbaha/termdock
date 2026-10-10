# ---- build stage ----
FROM golang:1.26-bookworm AS builder
WORKDIR /src

# Module downloads first for better layer caching.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Pure-Go SQLite (modernc.org/sqlite), so a static binary is fine.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/termdock ./cmd/termdock

# ---- runtime stage ----
FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/*

COPY --from=builder /out/termdock /usr/local/bin/termdock

ENV TERMDOCK_ADDR=:8080 \
    TERMDOCK_DATA=/data

VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/termdock"]
