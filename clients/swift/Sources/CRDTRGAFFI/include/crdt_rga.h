#ifndef DARKINNO_CRDT_RGA_SWIFT_BRIDGE_H
#define DARKINNO_CRDT_RGA_SWIFT_BRIDGE_H

// The Rust crate owns the ABI declaration. Keeping this bridge as an include
// rather than a copied header makes Swift compile against the same C contract.
#include "../../../../rust/include/crdt_rga.h"

#endif
