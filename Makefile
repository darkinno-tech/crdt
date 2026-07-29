.PHONY: fmt-check test test-unit test-integration test-extreme race vet fuzz coverage benchmark docker-test staticcheck lint verify

STATICCHECK ?= $(shell command -v staticcheck 2>/dev/null || printf '%s/bin/staticcheck' "$$(go env GOPATH)")
GOLANGCI_LINT ?= $(shell command -v golangci-lint 2>/dev/null || printf '%s/bin/golangci-lint' "$$(go env GOPATH)")
FUZZ_TIME ?= 10s
FUZZ_PARALLEL ?= 1

fmt-check:
	test -z "$$(gofmt -l .)"

test:
	go test ./...

test-unit:
	for package in . ./attachment ./clock ./counter ./delta ./encoding ./extensions ./lww ./merkle ./register ./replica ./set ./snapshot ./text ./tombstonegc ./tree ./cmd/crdt-analyze ./cmd/crdt-sync-probe ./examples/extensions-provider; do go test $$package; done

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
	go test -run=^$$ -fuzz=FuzzRGAUnmarshal -fuzztime=$(FUZZ_TIME) -parallel=$(FUZZ_PARALLEL) ./text
	go test -run=^$$ -fuzz=FuzzORTreeUnmarshal -fuzztime=$(FUZZ_TIME) -parallel=$(FUZZ_PARALLEL) ./tree

coverage:
	COVERAGE_THRESHOLD=90 ./scripts/check-coverage.sh

benchmark:
	go test -run='^$$' -bench=. -benchmem ./...

docker-test:
	docker build --build-arg GO_IMAGE=$${DOCKER_GO_IMAGE:-golang:1.26-bookworm} --file Dockerfile.ci --tag crdt-ci:local .
	docker run --rm --env COVERAGE_THRESHOLD=90 crdt-ci:local

staticcheck:
	$(STATICCHECK) ./...

lint:
	$(GOLANGCI_LINT) run ./...

verify: fmt-check test-unit test-integration test-extreme race vet fuzz coverage staticcheck lint
