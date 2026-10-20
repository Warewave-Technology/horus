# Build from the REPOSITORY ROOT (the operator imports ../pkg via a replace):
#   docker build -f operator/Dockerfile -t registry.example.com/horus-operator:dev .
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum* ./
COPY pkg/ pkg/
COPY operator/go.mod operator/go.sum* operator/
WORKDIR /src/operator
RUN go mod download
COPY operator/ /src/operator/
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/horus-operator ./cmd

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/horus-operator /horus-operator
USER 65532:65532
ENTRYPOINT ["/horus-operator"]
