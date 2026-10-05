FROM golang:1.27 AS build
WORKDIR /src
COPY . .
ARG TAGS=""
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -tags "$TAGS" -trimpath -ldflags "-s -w" -o /lumen ./cmd/lumen
# agent binaries served by the server at /download, plus checksums
RUN set -e; mkdir /dist; \
    for t in linux/amd64 linux/arm64 windows/amd64 windows/arm64; do \
      os=${t%/*}; arch=${t#*/}; ext=""; [ "$os" = windows ] && ext=".exe"; \
      CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
        -ldflags "-s -w -X github.com/danielingemar/lumen/internal/agent.Version=$VERSION" \
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
