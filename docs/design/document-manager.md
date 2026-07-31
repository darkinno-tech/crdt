# Document-level MoveRGA routing

`document.DocManager[T]` owns a bounded set of explicitly created
`list.MoveRGA[T]` documents. It validates document IDs, applies a
per-document list budget, and routes local `Insert`, `Delete`, `Move`, or
decoded remote deltas without a manager-wide document lock. Documents are
created before their deltas are accepted; a delta for an unknown ID is rejected
rather than allowing an untrusted peer to allocate retained state.

The manager is not a wire protocol or authorization scheme. Authenticate and
authorize the document ID, negotiate `list.MoveFrameType()`, then decode the
payload with `list.UnmarshalMoveDeltaWithLimits` before calling `ApplyDelta`.
Persist every document's HLC state atomically with its complete snapshot before
reusing the corresponding replica ID.

MoveRGA semantics version 2 may carry placement updates for the moved range
and its visible suffix, preserving exact sequential list semantics even when
the underlying RGA is a deep insertion chain. It consequently has `O(n)` local
metadata work in the worst case before projection; benchmark the real drag mix
and use a bounded document size instead of assuming constant-time moves.
