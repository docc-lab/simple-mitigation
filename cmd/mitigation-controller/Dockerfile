# Multi-stage build for the single-binary mitigation-controller DaemonSet.
# Builds the proto stubs inside the image so a fresh clone builds end-to-end.

FROM golang:1.23 AS build
WORKDIR /src

# protoc + plugins. The plugins are go-install'd in a separate layer so they
# cache independently of source changes.
RUN apt-get update \
 && apt-get install -y --no-install-recommends protobuf-compiler \
 && rm -rf /var/lib/apt/lists/*

ENV GOBIN=/usr/local/bin
RUN go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.34.2 \
 && go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1

# Cache go.mod/go.sum separately so source-only edits don't refetch deps.
COPY go.mod go.sum* ./
RUN go mod download || true

COPY . .

RUN make proto \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
      -o /out/mitigation-controller ./cmd/mitigation-controller

# Minimal runtime. The controller writes to host /sys/fs/cgroup, which
# requires root inside a privileged container -- so we use the regular
# (root-default) distroless/static image rather than :nonroot.
FROM gcr.io/distroless/static:latest
COPY --from=build /out/mitigation-controller /usr/local/bin/mitigation-controller
ENTRYPOINT ["/usr/local/bin/mitigation-controller"]
