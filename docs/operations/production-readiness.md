# Production configuration, errors, and telemetry

This module is a library. It does not read environment variables, mutate a
global logger, expose metrics over HTTP, or choose a deployment listener for a
host application. Those are application decisions. The APIs below give a host
one consistent, bounded way to wire them at the provider boundary.

## Layer explicit configuration over the environment

`config.Loader` resolves an ordered list of immutable sources. The first source
that contains a key wins. Use an explicit deployment source first, then the
environment; do not put a secret value into an error message or a log field.

```go
environment, err := config.NewEnvironment("CRDT_")
if err != nil { return err }
settings, err := config.New(
    config.NewMap(deploymentOverrides), // highest priority
    environment,                         // CRDT_MAX_EVENTS, CRDT_MAX_BYTES, ...
)
if err != nil { return err }

maxEvents, err := settings.Int("MAX_EVENTS", 100_000, 1, 10_000_000)
if err != nil { return err }
maxBytes, err := settings.Int("MAX_BYTES", 256<<20, 1<<20, 4<<30)
if err != nil { return err }
timeout, err := settings.Duration("WRITE_TIMEOUT", 10*time.Second)
if err != nil { return err }
```

Keys use portable upper-snake case. `Bool`, `Int`, and `Duration` are strict:
missing values use the supplied safe default, but malformed or out-of-range
values fail startup with `crdt.ErrorCodeInvalidConfig`. `Map` copies its input;
changing the original map after construction cannot race or change a running
configuration. `Loader` does not load `.env` files, watch files, or install
process-global state.

Apply the values explicitly to each constructor. For durable relays, keep the
existing bounds mandatory and configure TLS, authentication, origin patterns,
storage durability, and tenant quotas in the host as documented in the
[provider architecture guide](../integration/provider-architecture.md).

## Classify errors without breaking sentinel checks

`crdt.Error` adds a stable `ErrorCode` and constant operation name to an
underlying error. It unwraps, so a caller's existing sentinel checks remain
valid:

```go
handler, err := durable.NewHandler(config)
if errors.Is(err, durable.ErrInvalidConfig) {
    var detail *crdt.Error
    if errors.As(err, &detail) {
        // detail.Code == crdt.ErrorCodeInvalidConfig
        // detail.Operation == "durable.new_handler"
    }
    return err
}
```

The durable and extensions public constructors, plus the new configuration
helpers, use this form. Operation names are fixed diagnostic labels; they never contain peer IDs,
group IDs, endpoint URLs, headers, CRDT frames, or application values. Keep
using the package-specific sentinel for program control and use `ErrorCodeOf`
only for low-cardinality operational classification.

## Emit bounded, payload-free relay telemetry

`telemetry.Reporter` owns a bounded queue and invokes its `Sink` on a separate
goroutine. It is intentionally lossy: a slow sink never delays an append,
handshake, replay, CRDT mutation, or network callback. Export `Dropped()` as a
host metric and size the queue from a measured workload. Do not use telemetry
as a delivery receipt or audit log.

```go
reporter, err := telemetry.New(telemetry.Options{
    QueueSize: 512,
    Sink:      telemetry.SlogSink(logger), // standard log/slog JSON handler is typical
})
if err != nil { return err }
defer reporter.Close()

handler, err := durable.NewHandler(durable.Config{
    Store: store,
    Groups: groups,
    Authenticate: authenticate,
    Authorize: authorize,
    AuthorizeSubscription: authorizeSubscription,
    Telemetry: reporter,
})
```

The durable relay reports only these fixed operations: `handshake`, `replay`,
and `append`. The opt-in extensions relay reports `handshake`, `append`, and
`append_batch` on its server-side WebSocket/HTTP publication paths, and
`handshake` plus `append` on its native gRPC stream. Events contain a timestamp, duration, outcome, and low-cardinality
error code; they do not contain IDs, payloads, endpoint URLs, headers, raw
errors, or state summaries. A nil `durable.Config.Telemetry` retains the normal
no-reporter path.

`Close` returns immediately even if a third-party sink has blocked. A host may
wait on `Done()` when its sink is known to complete; it must not make shutdown
depend on an untrusted or remote telemetry backend.

The same reporter may be supplied explicitly to an `extensions.Config`; it is
still local process observation, not CRDT state, a client receipt, or a durable
audit stream:

```go
live, err := extensions.NewHandler(extensions.Config{
    Features: extensions.FeatureWebSocket | extensions.FeatureHTTP,
    Groups: groups,
    Authenticate: authenticate,
    Authorize: authorize,
    AuthorizeSubscription: authorizeSubscription,
    Telemetry: reporter,
})

grpcRelay, err := extensions.NewGRPCRelay(extensions.GRPCConfig{
    Groups: groups,
    Authenticate: authenticateGRPC,
    Authorize: authorize,
    AuthorizeSubscription: authorizeSubscription,
    Telemetry: reporter,
})
```

## Evidence and limits

```sh
# Unit, real loopback WebSocket, and configuration/error-path coverage.
go test ./config ./telemetry ./durable ./extensions
go test -race ./config ./telemetry ./durable ./extensions

# Parser robustness and hot-path allocation evidence.
go test -run='^$' -fuzz=FuzzLoaderTypedAccessors -fuzztime=20s ./config
go test -run='^$' -bench='Benchmark(ReporterRecord|HandlerRecord)' -benchmem ./telemetry ./durable
```

The durable test includes a real loopback WebSocket handshake, replay, and
append with a reporter sink. It proves event plumbing and that the relay's
existing authorization/manifest/append contracts remain intact. It does not
prove a production log backend, TLS termination, identity provider, external
metrics pipeline, target-machine p99 latency, or business authorization rules.
