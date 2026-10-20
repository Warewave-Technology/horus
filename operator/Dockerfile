# Build from the REPOSITORY ROOT (the operator imports ../pkg via a replace):
#   docker build -f operator/Dockerfile -t horus-operator:dev .
# Multi-arch (Go cross-compiles on the build platform, no emulation):
#   docker buildx build --platform linux/amd64,linux/arm64 -f operator/Dockerfile .
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum* ./
COPY pkg/ pkg/
COPY operator/go.mod operator/go.sum* operator/
WORKDIR /src/operator
RUN go mod download
COPY operator/ /src/operator/
ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/horus-operator ./cmd

FROM gcr.io/distroless/static:nonroot
ARG VERSION=dev
LABEL org.opencontainers.image.title="horus-operator" \
      org.opencontainers.image.description="Kubernetes operator that registers annotated Ingresses in a horus DNS server" \
      org.opencontainers.image.source="https://github.com/Warewave-Technology/horus" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}"
COPY --from=build /out/horus-operator /horus-operator
USER 65532:65532
ENTRYPOINT ["/horus-operator"]
