# Build context: repository root
FROM --platform=$BUILDPLATFORM golang:1.27.0-alpine3.24@sha256:4c9fe60190a2a3350ddc51de80d0224b8a6698d12bdfc999fee45ea9d6c46dbc AS builder
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION} -X main.distribution=docker" \
    -o /telrad-relay ./cmd/telrad-relay \
    && install -d -m 0700 /image-root/var/lib/telrad-relay \
    && install -m 0600 packaging/docker-relay.json /image-root/var/lib/telrad-relay/relay.json

FROM scratch
ARG VERSION=dev
ARG REVISION=unknown
ARG CREATED=unknown
LABEL org.opencontainers.image.title="Telrad Relay" \
    org.opencontainers.image.description="Outbound-only clinic DICOM and HL7 relay" \
    org.opencontainers.image.source="https://github.com/telrad-au/relay" \
    org.opencontainers.image.licenses="Apache-2.0" \
    org.opencontainers.image.version=$VERSION \
    org.opencontainers.image.revision=$REVISION \
    org.opencontainers.image.created=$CREATED
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /telrad-relay /usr/local/bin/telrad-relay
COPY --from=builder --chown=10001:10001 /image-root/var/lib/ /var/lib/
COPY LICENSE NOTICE THIRD_PARTY_NOTICES.md /usr/share/licenses/telrad-relay/
VOLUME ["/var/lib/telrad-relay"]
EXPOSE 11112/tcp 2575/tcp
USER 10001:10001
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["telrad-relay", "--config", "/var/lib/telrad-relay/relay.json", "ready"]
ENTRYPOINT ["telrad-relay", "--config", "/var/lib/telrad-relay/relay.json"]
CMD ["run"]
