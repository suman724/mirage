# Mirage — lazy workspace filesystem + client-initiated transport.
# See HANDOFF.md and docs/workspace-fs-and-transport.md.

# Module targets Go 1.25 (desync v1.0.1 + modern grpc require it). `auto` lets
# the go command select the matching toolchain (cached after first use).
GOTOOLCHAIN ?= auto
export GOTOOLCHAIN
# Generated-code plugins (protoc-gen-go/-go-grpc) live in the Go bin dir.
GOBIN := $(shell go env GOPATH)/bin
export PATH := $(GOBIN):$(PATH)

BIN := bin
# C compiler for the LD_PRELOAD shim (Linux-only).
CC ?= cc

.PHONY: all
all: tidy build test ## tidy, build, and test everything

.PHONY: proto
proto: ## regenerate Go from proto/ via buf (requires buf + protoc-gen-go[-grpc])
	buf generate

.PHONY: tools
tools: ## install the protoc plugins used by `make proto`
	go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

.PHONY: build
build: ## build both binaries into ./bin
	go build -o $(BIN)/mirage-server ./server
	go build -o $(BIN)/mirage-client ./client

.PHONY: shim-lib
shim-lib: ## build the LD_PRELOAD shim libmirageshim.so into ./bin (Linux only; for --shim mode)
	@mkdir -p $(BIN)
	$(CC) -shared -fPIC -O2 -Wall -Wextra -Werror -o $(BIN)/libmirageshim.so shim/mirageshim.c -ldl
	@echo "built $(BIN)/libmirageshim.so — use with: LD_PRELOAD=$(BIN)/libmirageshim.so MIRAGE_SHIM_ROOT=<root> MIRAGE_SHIM_SOCK=<sock>"

.PHONY: test
test: ## run all unit + integration tests
	go test ./...

.PHONY: integration
integration: ## run only the end-to-end integration test (verbose)
	go test -v -run TestEndToEnd ./test/...

.PHONY: test-race
test-race: ## run all tests under the race detector
	go test -race ./...

.PHONY: fuse-validate
fuse-validate: ## build a Linux image and run the live FUSE mount tests in it (needs Docker running)
	docker build -t mirage-fuse-validate -f Dockerfile .
	docker run --rm --cap-add SYS_ADMIN --device /dev/fuse \
		--security-opt apparmor:unconfined mirage-fuse-validate \
		go test -v -run 'TestLiveMount|TestFuseHarness' ./server/fuse/... ./test/...

.PHONY: fuse-demo
fuse-demo: ## scripted FUSE demo in a Linux container: cat a file off the mount and watch chunks fault (needs Docker)
	docker build -t mirage-fuse-validate -f Dockerfile .
	docker run --rm --cap-add SYS_ADMIN --device /dev/fuse \
		--security-opt apparmor:unconfined mirage-fuse-validate \
		bash scripts/fuse-demo.sh

.PHONY: fuse-shell
fuse-shell: ## interactive shell in a Linux container with the workspace FUSE-mounted; ls/cat and watch faults (needs Docker)
	docker build -t mirage-fuse-validate -f Dockerfile .
	docker run --rm -it --cap-add SYS_ADMIN --device /dev/fuse \
		--security-opt apparmor:unconfined mirage-fuse-validate \
		bash scripts/fuse-shell.sh

.PHONY: shim-validate
shim-validate: ## build a Linux image and run the Shimmer validation (C shim + libc tool matrix) — runs UNPRIVILEGED, no FUSE (needs Docker)
	docker build -t mirage-validate -f Dockerfile .
	docker run --rm mirage-validate bash scripts/shim-validate.sh

.PHONY: seccomp-validate
seccomp-validate: ## build a Linux image and run the seccomp interception validation (C launcher + Go supervisor, incl. a static Go binary) — UNPRIVILEGED (needs Docker)
	docker build -t mirage-validate -f Dockerfile .
	docker run --rm mirage-validate bash scripts/seccomp-validate.sh

.PHONY: seccomp-server-validate
seccomp-server-validate: ## the production path: real mirage-server --seccomp driven by mirage-client over gRPC + health endpoint — UNPRIVILEGED (needs Docker)
	docker build -t mirage-validate -f Dockerfile .
	docker run --rm mirage-validate bash scripts/seccomp-server-validate.sh

.PHONY: ptrace-validate
ptrace-validate: ## ptrace interception validation (C trace-launcher + Go tracer, open+exec traps + side-attach, incl. a static Go binary) — needs Docker. On a NATIVE amd64 host this also validates the amd64 register decode (QEMU emulation can't: it segfaults the toolchain and can't faithfully emulate ptrace).
	docker build -t mirage-validate -f Dockerfile .
	docker run --rm --cap-add SYS_PTRACE mirage-validate bash scripts/ptrace-validate.sh

.PHONY: ptrace-server-validate
ptrace-server-validate: ## the production path: real mirage-server --ptrace + mirage-client over gRPC, workload side-attaches from an independent process — needs Docker
	docker build -t mirage-validate -f Dockerfile .
	docker run --rm --cap-add SYS_PTRACE mirage-validate bash scripts/ptrace-server-validate.sh

.PHONY: mirage-trace-validate
mirage-trace-validate: ## validate the mirage_trace Python helper: it self-installs the RET_TRACE filter and the Go tracer materializes a workspace file (ctypes fallback path) — needs Docker
	docker build -t mirage-validate -f Dockerfile .
	docker run --rm --cap-add SYS_PTRACE mirage-validate bash scripts/mirage-trace-validate.sh

.PHONY: vet
vet: ## go vet
	go vet ./...

.PHONY: fmt
fmt: ## gofmt the tree
	gofmt -w .

.PHONY: tidy
tidy: ## sync go.mod/go.sum
	go mod tidy

# Convenience targets for a manual localhost demo (see README).
ADDR ?= 127.0.0.1:7777
DIR  ?= testdata/workspace
OUT  ?= ./mirage-out

.PHONY: run-server
run-server: build ## run the server (ACCEPTs the client connection)
	$(BIN)/mirage-server --addr $(ADDR) --out $(OUT)

.PHONY: run-client
run-client: build ## run the client (DIALs out, publishes $(DIR))
	$(BIN)/mirage-client --addr $(ADDR) --dir $(DIR)

.PHONY: clean
clean: ## remove build/output artifacts
	rm -rf $(BIN) ./mirage-out

.PHONY: help
help: ## list targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'
