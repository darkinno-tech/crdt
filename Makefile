.PHONY: fmt-check generate generate-check test test-unit test-integration test-extreme race vet fuzz fuzz-list fuzz-smoke coverage benchmark benchmark-regression docker-test staticcheck lint verify wasm wasm-v1 wasm-v1-test typescript-test wasm-test typescript-benchmark typescript-native-benchmark typescript-browser-benchmark typescript-bindings-benchmark wasm-benchmark wasm-bindings-benchmark sync-main

STATICCHECK ?= $(shell command -v staticcheck 2>/dev/null || printf '%s/bin/staticcheck' "$$(go env GOPATH)")
GOLANGCI_LINT ?= $(shell command -v golangci-lint 2>/dev/null || printf '%s/bin/golangci-lint' "$$(go env GOPATH)")
# A single fuzz worker avoids scheduler starvation in shared CI. Give it enough
# time to finish corpus minimization and a bounded large-frame decode before
# the fuzz context expires; 10s intermittently ended as context deadline.
FUZZ_TIME ?= 20s
FUZZ_PARALLEL ?= 1
WASM_DIR ?= .tmp/crdt-rga-wasm
WASM_RGA_PROTOCOL ?= run-v2
NPM ?= npm

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
	FUZZ_TIME="$(FUZZ_TIME)" FUZZ_PARALLEL="$(FUZZ_PARALLEL)" ./scripts/fuzz-all.sh

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
	go test -run=^$$ -fuzz=FuzzWire -fuzztime=$(FUZZ_TIME) -parallel=$(FUZZ_PARALLEL) ./durable
	go test -run=^$$ -fuzz=FuzzInboxHandlesUntrustedChangesWithoutPanic -fuzztime=$(FUZZ_TIME) -parallel=$(FUZZ_PARALLEL) ./replica

coverage:
	COVERAGE_THRESHOLD=90 ./scripts/check-coverage.sh

benchmark:
	go test -run='^$$' -bench=. -benchmem ./...

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

typescript-browser-benchmark:
	$(NPM) --prefix clients/typescript run bench:browser

typescript-bindings-benchmark:
	$(NPM) --prefix clients/typescript run bench:bindings

wasm-benchmark: wasm
	$(NPM) --prefix clients/typescript ci --ignore-scripts --prefer-offline
	CRDT_WASM_DIR="$(CURDIR)/$(WASM_DIR)" $(NPM) --prefix clients/typescript run bench:wasm

wasm-bindings-benchmark: wasm
	$(NPM) --prefix clients/typescript ci --ignore-scripts --prefer-offline
	CRDT_WASM_DIR="$(CURDIR)/$(WASM_DIR)" $(NPM) --prefix clients/typescript run bench:wasm-bindings

docker-test:
	docker build --build-arg GO_IMAGE=$${DOCKER_GO_IMAGE:-golang:1.26-bookworm} --file Dockerfile.ci --tag crdt-ci:local .
	docker run --rm --env COVERAGE_THRESHOLD=90 crdt-ci:local

staticcheck:
	$(STATICCHECK) ./...

lint:
	$(GOLANGCI_LINT) run ./...

verify: fmt-check generate-check test-unit test-integration test-extreme race vet fuzz coverage staticcheck lint

sync-main:
	./scripts/sync-main.sh
