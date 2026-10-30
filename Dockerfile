# Build the manager binary
# Release builds require digest-pinned overrides; local development may override explicitly.
ARG BASE_IMAGE=docker.io/library/golang:1.26.10@sha256:670bb5aa5349ae03b8516764718486f4f16399134bdc9acd0c46a23f043ab05c
ARG RUNTIME_IMAGE=gcr.io/distroless/static:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99
FROM ${BASE_IMAGE} AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the Go source (relies on .dockerignore to filter)
COPY . .

# Build
# the GOARCH has no default value to allow the binary to be built according to the host where the command
# was called. For example, if we call make docker-build in a local env which has the Apple Silicon M1 SO
# the docker BUILDPLATFORM arg will be linux/arm64 when for Apple x86 it will be linux/amd64. Therefore,
# by leaving it empty we can ensure that the container and binary shipped on it will have the same platform.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o manager cmd/main.go
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o registration-writer ./cmd/registration-writer
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o telegram-receiver ./cmd/telegram-receiver
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o telegram-delivery-worker ./cmd/telegram-delivery-worker
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o sentry-hook-producer ./cmd/sentry-hook-producer
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o sentry-hook-receiver ./cmd/sentry-hook-receiver

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM ${RUNTIME_IMAGE}
WORKDIR /
COPY --from=builder /workspace/manager .
COPY --from=builder /workspace/registration-writer .
COPY --from=builder /workspace/telegram-receiver .
COPY --from=builder /workspace/telegram-delivery-worker .
COPY --from=builder /workspace/sentry-hook-producer .
COPY --from=builder /workspace/sentry-hook-receiver .
USER 65532:65532

ENTRYPOINT ["/manager"]
