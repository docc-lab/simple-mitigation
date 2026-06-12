GO        ?= go
PROTOC    ?= protoc
MODULE    := github.com/coding-workspace/simple-mitigation-1
PROTO_DIR := proto
GEN_DIR   := gen/go/contentionpb

.PHONY: all build proto deps test clean tidy docker-horizontal docker-vertical

all: build

# Install the Go-side protoc plugins; protoc itself must be installed separately
# (`apt install protobuf-compiler` on Debian/Ubuntu).
deps:
	$(GO) install google.golang.org/protobuf/cmd/protoc-gen-go@v1.34.2
	$(GO) install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1

# Regenerate the contention.pb.go and contention_grpc.pb.go stubs.
# Must be run once after a fresh clone (gen/ is .gitignored).
proto:
	mkdir -p $(GEN_DIR)
	$(PROTOC) -I=$(PROTO_DIR) \
		--go_out=. --go_opt=module=$(MODULE) \
		--go-grpc_out=. --go-grpc_opt=module=$(MODULE) \
		$(PROTO_DIR)/contention.proto

tidy:
	$(GO) mod tidy

build:
	$(GO) build ./...

test:
	$(GO) test ./...

clean:
	rm -rf $(GEN_DIR) bin/

docker-horizontal:
	docker build -f cmd/horizontal-cpa-sidecar/Dockerfile -t simple-mitigation/horizontal-cpa-sidecar:dev .

docker-vertical:
	docker build -f cmd/vertical-scaler/Dockerfile -t simple-mitigation/vertical-scaler:dev .
