.PHONY: fmt-check test test-unit test-integration test-extreme race vet fuzz coverage benchmark docker-test staticcheck lint verify

STATICCHECK ?= $(shell command -v staticcheck 2>/dev/null || printf '%s/bin/staticcheck' "$$(go env GOPATH)")
GOLANGCI_LINT ?= $(shell command -v golangci-lint 2>/dev/null || printf '%s/bin/golangci-lint' "$$(go env GOPATH)")

fmt-check:
	test -z "$$(gofmt -l .)"

test:
	go test ./...

test-unit:
	for package in . ./clock ./counter ./delta ./encoding ./merkle ./set ./snapshot ./cmd/crdt-analyze ./cmd/crdt-sync-probe; do go test $$package; done

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
	for package in ./encoding ./counter ./set ./delta; do go test -run=^$$ -fuzz=Fuzz -fuzztime=10s $$package; done

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
