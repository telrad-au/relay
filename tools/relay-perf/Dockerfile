FROM golang:1.27.0-alpine3.24@sha256:4c9fe60190a2a3350ddc51de80d0224b8a6698d12bdfc999fee45ea9d6c46dbc AS builder
WORKDIR /src
COPY go.mod go.sum ./
COPY internal/synthetic ./internal/synthetic
COPY internal/retrieval ./internal/retrieval
COPY tools/relay-perf ./tools/relay-perf
ARG PERF_COVERAGE=false
RUN --mount=type=cache,target=/root/.cache/go-build \
    if [ "$PERF_COVERAGE" = true ]; then \
      CGO_ENABLED=0 go build -cover -covermode=atomic -coverpkg=./tools/relay-perf,./internal/synthetic -trimpath -o /relay-perf ./tools/relay-perf; \
    else CGO_ENABLED=0 go build -trimpath -o /relay-perf ./tools/relay-perf; fi

FROM golang:1.27.0-alpine3.24@sha256:4c9fe60190a2a3350ddc51de80d0224b8a6698d12bdfc999fee45ea9d6c46dbc
RUN apk add --no-cache iproute2=7.0.0-r0
COPY --from=builder /relay-perf /usr/local/bin/relay-perf
ENTRYPOINT ["relay-perf"]
