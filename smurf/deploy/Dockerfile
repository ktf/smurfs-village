# The smurf image: the smurf binary plus Litestream, nothing else.
#
#   docker build -f deploy/Dockerfile -t registry.cern.ch/alisw/smurf:<tag> .
#
# Built from the Go module root (smurfs-village/smurf). Litestream is pinned by version AND
# checksum (from its release's checksums.txt), since this container replicates
# the board's only durable copy.

FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags=-s -o /out/smurf ./cmd/smurf

FROM debian:trixie-slim AS litestream
ARG LITESTREAM_VERSION=0.5.17
ARG LITESTREAM_SHA256=cfb371176d164437ae869f8351cfde49bd1804ae71c61923f75c9cba9c9c006d
ADD https://github.com/benbjohnson/litestream/releases/download/v${LITESTREAM_VERSION}/litestream-${LITESTREAM_VERSION}-linux-x86_64.tar.gz /tmp/litestream.tgz
RUN echo "${LITESTREAM_SHA256}  /tmp/litestream.tgz" | sha256sum -c - \
 && tar -xzf /tmp/litestream.tgz -C /usr/local/bin litestream

FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/smurf /usr/local/bin/smurf
COPY --from=litestream /usr/local/bin/litestream /usr/local/bin/litestream
COPY deploy/entrypoint.sh /usr/local/bin/smurf-board-entrypoint
COPY deploy/litestream.yml /etc/litestream.yml
# Unprivileged; group 8485 = securityproxy_clients, which owns the sidecar's agent socket.
RUN groupadd --system --gid 8485 securityproxy_clients \
 && useradd --system --uid 8486 --gid 8485 --no-create-home smurf
USER smurf
ENTRYPOINT ["/usr/local/bin/smurf-board-entrypoint"]
