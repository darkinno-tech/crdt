# Go module layout and release procedure

The published root module, `github.com/DarkInno/crdt`, is the dependency-free
CRDT core. Durable storage, network transports, database providers, and
runnable examples are nested modules so selecting none of them adds no
third-party module requirements to a core consumer.

## Module boundary

| Module family | Scope |
| --- | --- |
| root `github.com/DarkInno/crdt` | CRDT types, codecs, replica helpers, and core tools; no third-party requirements. |
| `durable`, `persistence`, `telemetry`, `extensions` | Durable relay, checkpoint, telemetry, and transport features. |
| `providers/*` | Redis, PostgreSQL, MySQL, SQLite, WebRTC, and shared SQL relay implementation. |
| `examples` | Runnable examples, including the WebSocket reference. |

`go.work` is a checkout-only development workspace. Its version-specific
`replace` directives make the untagged source modules resolve to one another.
They are not part of a downstream consumer's module graph; each published
submodule's `go.mod` deliberately contains no `replace` directive.

## Stable release invariant

The first split release is `v1.0.32`. Its root and every nested module must be
tagged from the same commit:

```text
v1.0.32
durable/v1.0.32
persistence/v1.0.32
telemetry/v1.0.32
extensions/v1.0.32
providers/mysql/v1.0.32
providers/sqlite/v1.0.32
...and every other nested module
```

The matching root tag is required before a consumer can resolve a nested
module: earlier root releases still include these directories and would make
the import path ambiguous. All internal `github.com/DarkInno/crdt...` requires
in a split-release commit therefore use the exact release version.

The stable release workflow calls `scripts/tag-go-modules.sh`; it verifies
those internal versions, refuses to move an existing tag, and creates/pushes
the root and nested tags atomically in one push. Do not create just the root
tag or a provider tag by hand.

Before merging the stable release candidate, run from the checkout:

```sh
make test
make race
make vet
make coverage
```

After the first tags have reached the module proxy, verify a clean consumer
with `GOWORK=off`. Do not use that mode before the tags exist: the development
workspace is intentionally what resolves the untagged module family.
