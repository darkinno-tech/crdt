# Cross-Host Probe Deployment Runbook

This is the English source runbook. The Chinese translation is maintained in
`cross-host-probe.zh-CN.md`.

## Purpose and scope

`crdt` is a library, not a network service. `cmd/crdt-sync-probe` exists only
to validate encoded delta delivery across controlled hosts. It is not a
production replication daemon and does not add TLS, membership, persistence,
authorization policy, or retry semantics to the library.

Use this runbook for a short, controlled test window. Do not use real
credentials, production CRDT state, or publicly reusable tokens in examples or
shell history.

## Prerequisites

- A verified local checkout with supported Go 1.26.x (the module language minimum remains Go 1.21).
- Two Linux amd64 test hosts reachable over SSH. Use a dedicated non-root
  account where possible; the commands below use `/opt/crdt-e2e` only as an
  example deployment directory.
- A network policy that permits the temporary probe port only between the test
  participants. Prefer private networking or SSH forwarding.
- `openssl`, `sha256sum`, and `curl` available on the test hosts.

## 1. Verify and build locally

Run the library gates before publishing a probe binary:

```sh
make test-unit test-integration
make test-extreme
go test -race ./...
make coverage
go vet ./...
```

Build static Linux binaries locally; this avoids installing a compiler on the
test hosts:

```sh
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  go build -trimpath -ldflags='-s -w' \
  -o ./dist/crdt-sync-probe ./cmd/crdt-sync-probe

GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  go build -trimpath -ldflags='-s -w' \
  -o ./dist/crdt-analyze ./cmd/crdt-analyze

sha256sum ./dist/crdt-sync-probe ./dist/crdt-analyze
```

Record the two hashes and verify them after every upload. Do not copy a binary
whose hash differs from the locally recorded value.

## 2. Create a short-lived token

Generate a new token for this single exercise. Keep it out of command-line
arguments and version control.

```sh
umask 077
openssl rand -hex 32 > ./probe.token
chmod 600 ./probe.token
```

The probe accepts `-token-file`; prefer it over `-token` because process lists
and shell history can expose arguments.

## 3. Install on each test host

Replace placeholders before executing. Never place passwords or tokens in this
document or in source control.

```sh
ssh <user>@<host-a> 'install -d -m 700 /opt/crdt-e2e'
scp ./dist/crdt-sync-probe ./dist/crdt-analyze ./probe.token \
  <user>@<host-a>:/opt/crdt-e2e/

ssh <user>@<host-a> '
  cd /opt/crdt-e2e &&
  chmod 700 crdt-sync-probe crdt-analyze &&
  chmod 600 probe.token &&
  sha256sum crdt-sync-probe crdt-analyze
'
```

Repeat for host B and compare the remote hashes with the local values. Keep the
directory readable only by the deployment account.

## 4. Run the two receivers

The default listener is `127.0.0.1:49511`. Bind `0.0.0.0:49511` only for a
temporary, firewall-restricted cross-host test.

```sh
cd /opt/crdt-e2e
nohup ./crdt-sync-probe \
  -mode serve \
  -listen 0.0.0.0:49511 \
  -replica host-a \
  -token-file ./probe.token \
  > server.log 2>&1 &
echo $!
```

On host B use a distinct `-replica` value. Record each PID. Before any public
binding, restrict inbound access to the two test hosts and the local sender.

## 5. Exercise delivery scenarios

Run each sender with a globally unique replica ID. A comma-separated target
list generates one counter delta and one OR-Set delta, then sends those same
bytes to every listed receiver.

```sh
./crdt-sync-probe \
  -mode send \
  -target http://<host-a>:49511,http://<host-b>:49511 \
  -replica sender-a \
  -token-file ./probe.token \
  -counter-increment 2 \
  -element alpha \
  -duplicates 11 \
  -timeout 15s
```

Repeat from host B and the local machine with distinct IDs and elements. The
returned JSON from both targets must have identical counter component maps and
the same sorted element set.

Validate negative paths on each receiver:

- Request `GET /state` without the token: expect HTTP `401`.
- Send a non-frame body to `POST /counter` with a valid token: expect `400`.
- Send a body larger than 1 MiB with a valid token: expect `400`.
- Confirm valid state is unchanged after each rejected request.

For a local capacity gate before a cross-host exercise, `make test-extreme`
checks three replicas holding 6,144 total OR-Set elements, state merge,
snapshot recovery, duplicate post-recovery delivery, Merkle equality, and a
256-component G-Counter batch in normal and race-instrumented modes.

On the 2026-07-28 local verification, ten race-instrumented repetitions
produced 150,830–151,409 byte OR-Set state frames and a 7,047 byte counter
batch, with no failure. These are test observations, not a universal production
capacity promise; select transport and decoder limits from the application's
own payload, membership, latency, and memory budgets.

## 6. Analyze a captured frame

Only analyze frames already bounded by the transport and stored in a protected
test location:

```sh
./crdt-analyze -file ./captured.frame -max-bytes 1048576
```

The JSON report validates the outer frame before emitting its type, codec ID,
payload size, and SHA-256 fingerprint. It does not validate a type-specific
payload or authenticate the frame.

## 7. Stop and verify cleanup

Stop exactly the PIDs recorded in step 4; do not use broad process matching on
shared hosts.

```sh
kill <host-a-probe-pid>
kill <host-b-probe-pid>
ss -ltn 'sport = :49511'
```

The final `ss` commands must show no listener. Remove or rotate `probe.token`
after the test. Keep the binaries only if the protected test environment needs
them again; otherwise remove the exact deployment directory through the host's
approved change process.

## Acceptance record template

| Item | Required evidence |
| --- | --- |
| Binary integrity | Local and both remote SHA-256 values match |
| Delivery idempotency | Repeated delta leaves one counter component and one set membership |
| Multi-target consistency | Both receivers return equal state for the same broadcast delta |
| Input protection | Unauthorized = 401; malformed and oversized bodies = 400 |
| Exposure cleanup | Recorded PIDs stopped; no probe listener remains |
