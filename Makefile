.PHONY: fmt-check test test-unit test-integration test-extreme race vet fuzz fuzz-smoke coverage benchmark docker-test staticcheck lint verify wasm wasm-v1 wasm-v1-test typescript-test wasm-test typescript-benchmark typescript-native-benchmark typescript-browser-benchmark wasm-benchmark sync-main

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
	go test -run=^$$ -fuzz=FuzzUnmarshalDelta -fuzztime=$(FUZZ_TIME) -parallel=$(FUZZ_PARALLEL) ./attachment
	go test -run=^$$ -fuzz=FuzzReferenceVerify -fuzztime=$(FUZZ_TIME) -parallel=$(FUZZ_PARALLEL) ./attachment
	go test -run=^$$ -fuzz=FuzzUnmarshalUpdate -fuzztime=$(FUZZ_TIME) -parallel=$(FUZZ_PARALLEL) ./awareness
	go test -run=^$$ -fuzz=Fuzz -fuzztime=$(FUZZ_TIME) -parallel=$(FUZZ_PARALLEL) ./encoding
	go test -run=^$$ -fuzz=FuzzGCounterUnmarshalBinary -fuzztime=$(FUZZ_TIME) -parallel=$(FUZZ_PARALLEL) ./counter
	go test -run=^$$ -fuzz=FuzzPNCounterUnmarshalBinary -fuzztime=$(FUZZ_TIME) -parallel=$(FUZZ_PARALLEL) ./counter
	go test -run=^$$ -fuzz=FuzzMapUnmarshal -fuzztime=$(FUZZ_TIME) -parallel=$(FUZZ_PARALLEL) ./lww
	go test -run=^$$ -fuzz=FuzzGSetUnmarshalBinary -fuzztime=$(FUZZ_TIME) -parallel=$(FUZZ_PARALLEL) ./set
	go test -run=^$$ -fuzz=FuzzORSetUnmarshalBinary -fuzztime=$(FUZZ_TIME) -parallel=$(FUZZ_PARALLEL) ./set
	go test -run=^$$ -fuzz=FuzzMVRegisterUnmarshal -fuzztime=$(FUZZ_TIME) -parallel=$(FUZZ_PARALLEL) ./register
	go test -run=^$$ -fuzz=Fuzz -fuzztime=$(FUZZ_TIME) -parallel=$(FUZZ_PARALLEL) ./delta
	go test -run=^$$ -fuzz=FuzzInboxHandlesUntrustedChangesWithoutPanic -fuzztime=$(FUZZ_TIME) -parallel=$(FUZZ_PARALLEL) ./replica
	go test -run=^$$ -fuzz=FuzzWireDecoders -fuzztime=$(FUZZ_TIME) -parallel=$(FUZZ_PARALLEL) ./extensions
	go test -run=^$$ -fuzz=FuzzWire -fuzztime=$(FUZZ_TIME) -parallel=$(FUZZ_PARALLEL) ./durable
	go test -run=^$$ -fuzz=FuzzUnmarshalCheckpoint -fuzztime=$(FUZZ_TIME) -parallel=$(FUZZ_PARALLEL) ./persistence
	go test -run=^$$ -fuzz=FuzzRGAUnmarshal -fuzztime=$(FUZZ_TIME) -parallel=$(FUZZ_PARALLEL) ./text
	go test -run=^$$ -fuzz=FuzzRGARunUnmarshal -fuzztime=$(FUZZ_TIME) -parallel=$(FUZZ_PARALLEL) ./text
	go test -run=^$$ -fuzz=FuzzRGAUnmarshal -fuzztime=$(FUZZ_TIME) -parallel=$(FUZZ_PARALLEL) ./list
	go test -run=^$$ -fuzz=FuzzParseDocument -fuzztime=$(FUZZ_TIME) -parallel=$(FUZZ_PARALLEL) ./xml
	go test -run=^$$ -fuzz=FuzzUnmarshal -fuzztime=$(FUZZ_TIME) -parallel=$(FUZZ_PARALLEL) ./richtext
	go test -run=^$$ -fuzz=FuzzORTreeUnmarshal -fuzztime=$(FUZZ_TIME) -parallel=$(FUZZ_PARALLEL) ./tree

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

wasm:
	mkdir -p "$(WASM_DIR)"
	GOOS=js GOARCH=wasm go build -trimpath -ldflags='-s -w -X main.wasmWireFormat=$(WASM_RGA_PROTOCOL)' -o "$(WASM_DIR)/crdt-rga.wasm" ./cmd/crdt-rga-wasm
	cp "$$(go env GOROOT)/lib/wasm/wasm_exec.js" "$(WASM_DIR)/wasm_exec.js"

wasm-v1:
	$(MAKE) wasm WASM_RGA_PROTOCOL=v1 WASM_DIR=.tmp/crdt-rga-v1-wasm

wasm-v1-test: wasm-v1
	$(NPM) --prefix clients/typescript ci --ignore-scripts
	CRDT_WASM_DIR="$(CURDIR)/.tmp/crdt-rga-v1-wasm" CRDT_RGA_PROTOCOL=v1 $(NPM) --prefix clients/typescript run test:compat

typescript-test:
	$(NPM) --prefix clients/typescript ci --ignore-scripts
	$(NPM) --prefix clients/typescript run test

wasm-test: wasm
	$(NPM) --prefix clients/typescript ci --ignore-scripts
	CRDT_WASM_DIR="$(CURDIR)/$(WASM_DIR)" $(NPM) --prefix clients/typescript run test:compat

typescript-benchmark:
	$(NPM) --prefix clients/typescript run bench:frame

typescript-native-benchmark:
	$(NPM) --prefix clients/typescript run bench:native

typescript-browser-benchmark:
	$(NPM) --prefix clients/typescript run bench:browser

wasm-benchmark: wasm
	$(NPM) --prefix clients/typescript ci --ignore-scripts
	CRDT_WASM_DIR="$(CURDIR)/$(WASM_DIR)" $(NPM) --prefix clients/typescript run bench:wasm

docker-test:
	docker build --build-arg GO_IMAGE=$${DOCKER_GO_IMAGE:-golang:1.26-bookworm} --file Dockerfile.ci --tag crdt-ci:local .
	docker run --rm --env COVERAGE_THRESHOLD=90 crdt-ci:local

staticcheck:
	$(STATICCHECK) ./...

lint:
	$(GOLANGCI_LINT) run ./...

verify: fmt-check test-unit test-integration test-extreme race vet fuzz coverage staticcheck lint

sync-main:
	./scripts/sync-main.sh
