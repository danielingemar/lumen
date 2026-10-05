FROM golang:1.22 AS build
WORKDIR /src
COPY . .
ARG TAGS=""
ARG VERSION=dev
# The version identifies the source code: the same source gives the same version on every server and in every agent,
# so the web UI and the Hosts page can show whether a server or an agent is up to date. Override with --build-arg VERSION=1.2.3
RUN set -e; \
    if [ "$VERSION" = dev ]; then \
      VERSION="src-$(find . -type f \( -name '*.go' -o -name '*.html' -o -name go.mod \) ! -name '*_test.go' | LC_ALL=C sort | xargs cat | sha256sum | cut -c1-8)"; \
    fi; \
    echo "$VERSION" > /version
RUN CGO_ENABLED=0 go build -tags "$TAGS" -trimpath -ldflags "-s -w -X github.com/danielingemar/lumen/internal/buildinfo.Version=$(cat /version)" -o /lumen ./cmd/lumen
# agent binaries served by the server at /download, plus checksums
RUN set -e; mkdir /dist; V="$(cat /version)"; \
    for t in linux/amd64 linux/arm64 windows/amd64 windows/arm64; do \
      os=${t%/*}; arch=${t#*/}; ext=""; [ "$os" = windows ] && ext=".exe"; \
      CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
        -ldflags "-s -w -X github.com/danielingemar/lumen/internal/agent.Version=$V" \
        -o /dist/lumen-agent-$os-$arch$ext ./cmd/lumen-agent; \
    done; \
    mkdir /data /backup; cd /dist && sha256sum lumen-agent-* > /tmp/SHA256SUMS && mv /tmp/SHA256SUMS /dist/SHA256SUMS

FROM gcr.io/distroless/static-debian12:nonroot
ENV LUMEN_DATA_DIR=/data
COPY --from=build /lumen /lumen
COPY --from=build --chown=65532:65532 /data /data
COPY --from=build --chown=65532:65532 /backup /backup
COPY --from=build /dist /dist
EXPOSE 4318
ENTRYPOINT ["/lumen"]
