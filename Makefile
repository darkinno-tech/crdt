.PHONY: fmt-check generate generate-check test test-unit test-integration test-extreme race vet fuzz fuzz-list fuzz-smoke coverage benchmark benchmark-regression yjs-store-test yjs-store-benchmark docker-test staticcheck lint verify formal-rga wasm wasm-v1 wasm-v1-test typescript-test wasm-test typescript-benchmark typescript-native-benchmark typescript-browser-benchmark typescript-bindings-benchmark wasm-benchmark wasm-browser-benchmark wasm-bindings-benchmark rust-test rust-benchmark python-test swift-test cpp-test cpp-benchmark sync-main

STATICCHECK ?= $(shell command -v staticcheck 2>/dev/null || printf '%s/bin/staticcheck' "$$(go env GOPATH)")
GOLANGCI_LINT ?= $(shell command -v golangci-lint 2>/dev/null || printf '%s/bin/golangci-lint' "$$(go env GOPATH)")
# A single fuzz worker avoids scheduler starvation in shared CI. A bounded 25s
# window leaves time for corpus minimization and clean coordinator shutdown;
# 20s intermittently ended as context deadline.
FUZZ_TIME ?= 25s
FUZZ_PARALLEL ?= 1
# XML and document-tree fuzzers start from larger valid-state corpora and can
# need extra time for Go's fuzz coordinator to quiesce after the requested
# window. Keep this targeted grace period separate so ordinary decoder fuzzing
# remains fast.
FUZZ_XML_TIME ?= 45s
FUZZ_DOCUMENTTREE_TIME ?= 30s
FUZZ_ENCODING_TIME ?= 30s
WASM_DIR ?= .tmp/crdt-rga-wasm
WASM_RGA_PROTOCOL ?= run-v2
NPM ?= npm
RUST_MANIFEST ?= clients/rust/Cargo.toml
RUST_LIBRARY_DIR ?= $(CURDIR)/clients/rust/target/debug
RUST_LIBRARY_EXTENSION ?= $(shell uname -s | sed -e 's/^Darwin$$/dylib/' -e 's/^Linux$$/so/')
RUST_LIBRARY_NAME ?= libdarkinno_crdt_rga.$(RUST_LIBRARY_EXTENSION)
RUST_RELEASE_LIBRARY_DIR ?= $(CURDIR)/clients/rust/target/release
CPP_COMPILER ?= c++
CPP_FLAGS ?= -std=c++20 -Wall -Wextra -Werror -pedantic
CPP_BUILD_DIR ?= clients/cpp/.build
CPP_TEST_BINARY ?= $(CPP_BUILD_DIR)/crdt-rga-cpp-conformance
CPP_BENCHMARK_BINARY ?= $(CPP_BUILD_DIR)/crdt-rga-cpp-benchmark

ifeq ($(shell uname -s),Darwin)
CPP_LIBRARY_PATH_ENV := DYLD_LIBRARY_PATH
else
CPP_LIBRARY_PATH_ENV := LD_LIBRARY_PATH
endif

fmt-check:
	test -z "$$(gofmt -l .)"

generate:
	go generate ./...

generate-check:
	go run ./internal/cmd/typeidgen -check

test:
	go test ./...

test-unit:
	go test $$(go list ./...)

test-integration:
	go test -count=1 -run '^TestThreeReplicaDeltaDeliveryRecoveryAndAntiEntropy$$' .

test-extreme:
	go test -count=1 -run '^TestHighCardinalityThreeReplicaRecoveryAndConvergence$$' .
	go test -race -count=1 -run '^TestHighCardinalityThreeReplicaRecoveryAndConvergence$$' .

race:
	go test -race ./...

vet:
	go vet ./...

fuzz:
	FUZZ_TIME="$(FUZZ_TIME)" FUZZ_XML_TIME="$(FUZZ_XML_TIME)" FUZZ_DOCUMENTTREE_TIME="$(FUZZ_DOCUMENTTREE_TIME)" FUZZ_ENCODING_TIME="$(FUZZ_ENCODING_TIME)" FUZZ_PARALLEL="$(FUZZ_PARALLEL)" ./scripts/fuzz-all.sh

# List the release fuzz coverage derived from the current package graph. It is
# intentionally separate from fuzz-smoke, whose small curated list documents
# the most important attacker-controlled trust boundaries for pull requests.
fuzz-list:
	FUZZ_LIST_ONLY=1 ./scripts/fuzz-all.sh

# Fuzz the independent trust boundaries that most often accept attacker-controlled
# bytes in a pull request. Release candidates still run the complete fuzz target
# above, keeping the faster PR feedback path distinct from release validation.
fuzz-smoke:
	go test -run=^$$ -fuzz=FuzzUnmarshalDelta -fuzztime=$(FUZZ_TIME) -parallel=$(FUZZ_PARALLEL) ./attachment
	go test -run=^$$ -fuzz=FuzzReferenceVerify -fuzztime=$(FUZZ_TIME) -parallel=$(FUZZ_PARALLEL) ./attachment
	go test -run=^$$ -fuzz=FuzzUnmarshalUpdate -fuzztime=$(FUZZ_TIME) -parallel=$(FUZZ_PARALLEL) ./awareness
	go test -run=^$$ -fuzz=FuzzDocumentTreeWire -fuzztime=$(FUZZ_DOCUMENTTREE_TIME) -parallel=$(FUZZ_PARALLEL) ./documenttree
	go test -run=^$$ -fuzz=FuzzWire -fuzztime=$(FUZZ_TIME) -parallel=$(FUZZ_PARALLEL) ./durable
	go test -run=^$$ -fuzz=FuzzInboxHandlesUntrustedChangesWithoutPanic -fuzztime=$(FUZZ_TIME) -parallel=$(FUZZ_PARALLEL) ./replica

coverage:
	COVERAGE_THRESHOLD=90 ./scripts/check-coverage.sh

benchmark:
	go test -run='^$$' -bench=. -benchmem ./...

# Level 1 interoperability needs the maintained Yjs engine. These targets are
# separate from Go-only checks so library consumers do not need Node merely to
# compile this module. They install the lockfile exactly, then exercise both
# direct real-Yjs scenarios and the Go-to-sidecar HTTP contract.
yjs-store-test:
	$(NPM) --prefix yjsstore/runtime ci --ignore-scripts --prefer-offline
	$(NPM) --prefix yjsstore/runtime test
	CRDT_YJS_STORE_NODE_INTEGRATION=1 go test -count=1 ./extensions -run '^TestYJSStoreNodeSidecarIntegration$$'

yjs-store-benchmark:
	$(NPM) --prefix yjsstore/runtime ci --ignore-scripts --prefer-offline
	$(NPM) --prefix yjsstore/runtime run bench

# BENCHMARK_BASE must be a checkout of the commit to compare against. The
# candidate and baseline run consecutively with one logical processor so their
# medians are comparable; this detects regressions, not production capacity.
benchmark-regression:
	@test -n "$(BENCHMARK_BASE)" || (echo "set BENCHMARK_BASE to a baseline checkout" >&2; exit 2)
	@mkdir -p .tmp/benchmark-results
	@(cd "$(BENCHMARK_BASE)" && GOMAXPROCS=1 go test -run='^$$' -bench='^(BenchmarkGCounterApplyDelta|BenchmarkRGAApplyDeltaLinearChain|BenchmarkProviderEndToEndRelayFanout)$$' -benchmem -benchtime=100ms -count=5 -cpu=1 ./counter ./text ./examples/websocket-provider/provider) > .tmp/benchmark-results/baseline.txt
	@GOMAXPROCS=1 go test -run='^$$' -bench='^(BenchmarkGCounterApplyDelta|BenchmarkRGAApplyDeltaLinearChain|BenchmarkProviderEndToEndRelayFanout)$$' -benchmem -benchtime=100ms -count=5 -cpu=1 ./counter ./text ./examples/websocket-provider/provider > .tmp/benchmark-results/candidate.txt
	@go run ./cmd/crdt-benchmark-check -base .tmp/benchmark-results/baseline.txt -candidate .tmp/benchmark-results/candidate.txt -minimum-samples 5 -max-time-regression 1.00 -max-bytes-regression 0.05 -max-allocs-regression 0.05 -require BenchmarkGCounterApplyDelta -require BenchmarkRGAApplyDeltaLinearChain -require BenchmarkProviderEndToEndRelayFanout/receivers_1 -require BenchmarkProviderEndToEndRelayFanout/receivers_4 -require BenchmarkProviderEndToEndRelayFanout/receivers_16

wasm:
	mkdir -p "$(WASM_DIR)"
	GOOS=js GOARCH=wasm go build -trimpath -ldflags='-s -w -X main.wasmWireFormat=$(WASM_RGA_PROTOCOL)' -o "$(WASM_DIR)/crdt-rga.wasm" ./cmd/crdt-rga-wasm
	cp "$$(go env GOROOT)/lib/wasm/wasm_exec.js" "$(WASM_DIR)/wasm_exec.js"

wasm-v1:
	$(MAKE) wasm WASM_RGA_PROTOCOL=v1 WASM_DIR=.tmp/crdt-rga-v1-wasm

wasm-v1-test: wasm-v1
	$(NPM) --prefix clients/typescript ci --ignore-scripts --prefer-offline
	CRDT_WASM_DIR="$(CURDIR)/.tmp/crdt-rga-v1-wasm" CRDT_RGA_PROTOCOL=v1 $(NPM) --prefix clients/typescript run test:compat

typescript-test:
	$(NPM) --prefix clients/typescript ci --ignore-scripts --prefer-offline
	$(NPM) --prefix clients/typescript run test

wasm-test: wasm
	$(NPM) --prefix clients/typescript ci --ignore-scripts --prefer-offline
	CRDT_WASM_DIR="$(CURDIR)/$(WASM_DIR)" $(NPM) --prefix clients/typescript run test:compat

typescript-benchmark:
	$(NPM) --prefix clients/typescript run bench:frame

typescript-native-benchmark:
	$(NPM) --prefix clients/typescript run bench:native

typescript-collections-benchmark:
	$(NPM) --prefix clients/typescript run bench:collections

typescript-browser-benchmark:
	$(NPM) --prefix clients/typescript run bench:browser

typescript-bindings-benchmark:
	$(NPM) --prefix clients/typescript run bench:bindings

typescript-blocknote-benchmark:
	$(NPM) --prefix clients/typescript run bench:blocknote

wasm-benchmark: wasm
	$(NPM) --prefix clients/typescript ci --ignore-scripts --prefer-offline
	CRDT_WASM_DIR="$(CURDIR)/$(WASM_DIR)" $(NPM) --prefix clients/typescript run bench:wasm

wasm-browser-benchmark: wasm
	$(NPM) --prefix clients/typescript ci --ignore-scripts --prefer-offline
	CRDT_WASM_DIR="$(CURDIR)/$(WASM_DIR)" $(NPM) --prefix clients/typescript run bench:wasm-browser

wasm-bindings-benchmark: wasm
	$(NPM) --prefix clients/typescript ci --ignore-scripts --prefer-offline
	CRDT_WASM_DIR="$(CURDIR)/$(WASM_DIR)" $(NPM) --prefix clients/typescript run bench:wasm-bindings

# Native language clients share the Rust run-v2 core and its C ABI. They are
# intentionally optional: the Go-only verify/docker images do not install the
# Rust, Python, or Swift toolchains. Release candidates run these explicitly.
rust-test:
	cargo fmt --manifest-path "$(RUST_MANIFEST)" -- --check
	cargo test --manifest-path "$(RUST_MANIFEST)"
	cargo clippy --manifest-path "$(RUST_MANIFEST)" --all-targets -- -D warnings

rust-benchmark:
	cargo bench --manifest-path "$(RUST_MANIFEST)" --bench rga

python-test:
	cargo build --manifest-path "$(RUST_MANIFEST)"
	CRDT_RGA_LIBRARY="$(RUST_LIBRARY_DIR)/$(RUST_LIBRARY_NAME)" python3 -m unittest clients/python/tests/test_rga.py -v

swift-test:
	@test "$$(uname -s)" = Darwin || (echo "swift-test requires a Darwin Rust dynamic library" >&2; exit 2)
	cargo build --manifest-path "$(RUST_MANIFEST)"
	CRDT_RGA_LIBRARY_DIR="$(RUST_LIBRARY_DIR)" DYLD_LIBRARY_PATH="$(RUST_LIBRARY_DIR)" swift run --package-path clients/swift crdt-rga-swift-conformance

# The C++20 facade is an owned-handle binding over the same Rust run-v2 core.
# It intentionally has no package manager or generated source dependency.
cpp-test:
	cargo build --manifest-path "$(RUST_MANIFEST)"
	mkdir -p "$(CPP_BUILD_DIR)"
	$(CPP_COMPILER) $(CPP_FLAGS) -Iclients/cpp/include -Iclients/rust/include clients/cpp/tests/conformance.cpp -L"$(RUST_LIBRARY_DIR)" -ldarkinno_crdt_rga -o "$(CPP_TEST_BINARY)"
	$(CPP_LIBRARY_PATH_ENV)="$(RUST_LIBRARY_DIR)" "$(CPP_TEST_BINARY)"

cpp-benchmark:
	cargo build --release --manifest-path "$(RUST_MANIFEST)"
	mkdir -p "$(CPP_BUILD_DIR)"
	$(CPP_COMPILER) $(CPP_FLAGS) -O3 -Iclients/cpp/include -Iclients/rust/include clients/cpp/bench/rga.cpp -L"$(RUST_RELEASE_LIBRARY_DIR)" -ldarkinno_crdt_rga -o "$(CPP_BENCHMARK_BINARY)"
	$(CPP_LIBRARY_PATH_ENV)="$(RUST_RELEASE_LIBRARY_DIR)" "$(CPP_BENCHMARK_BINARY)"

docker-test:
	docker build --build-arg GO_IMAGE=$${DOCKER_GO_IMAGE:-golang:1.26-bookworm} --file Dockerfile.ci --tag crdt-ci:local .
	docker run --rm --env COVERAGE_THRESHOLD=90 crdt-ci:local

staticcheck:
	$(STATICCHECK) ./...

lint:
	$(GOLANGCI_LINT) run ./...

# The formal model is an explicitly invoked, pinned Lean check. It remains
# outside `verify` until the repository adopts a pinned Lean CI bootstrap.
formal-rga:
	cd formal/rga && lake build

verify: fmt-check generate-check test-unit test-integration test-extreme race vet fuzz coverage staticcheck lint

sync-main:
	./scripts/sync-main.sh
