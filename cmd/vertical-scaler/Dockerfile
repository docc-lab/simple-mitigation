# syntax=docker/dockerfile:1.6
FROM golang:1.23 AS build
RUN apt-get update \
 && apt-get install -y --no-install-recommends protobuf-compiler \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY go.mod ./
COPY go.sum* ./
COPY . .
RUN make deps \
 && make proto \
 && go mod tidy \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
      -o /out/vertical-scaler ./cmd/vertical-scaler

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/vertical-scaler /vertical-scaler
USER nonroot:nonroot
ENTRYPOINT ["/vertical-scaler"]
