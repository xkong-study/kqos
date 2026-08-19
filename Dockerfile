# All three kqos binaries share one image. They are a few megabytes each and
# always deployed together, so a single image means one build, one push and one
# tag to reason about when debugging a version skew.
FROM golang:1.26-alpine AS builder

ARG TARGETARCH=arm64
ARG VERSION=dev

WORKDIR /src

# Dependencies first, so a source-only change does not re-download the module
# graph. This is the difference between a 4-second rebuild and a 90-second one.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY pkg/ pkg/

RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build \
      -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/kqos-agent ./cmd/kqos-agent && \
    CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build \
      -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/kqos-controller ./cmd/kqos-controller && \
    CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build \
      -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/kqos-webhook ./cmd/kqos-webhook

# Alpine rather than distroless on purpose: this project's whole subject is
# what the kernel reports in /sys/fs/cgroup, and being able to exec into the
# agent and read those files by hand is worth the extra four megabytes.
FROM alpine:3.22

RUN apk add --no-cache ca-certificates

COPY --from=builder /out/kqos-agent /usr/local/bin/kqos-agent
COPY --from=builder /out/kqos-controller /usr/local/bin/kqos-controller
COPY --from=builder /out/kqos-webhook /usr/local/bin/kqos-webhook

ENTRYPOINT ["/usr/local/bin/kqos-agent"]
