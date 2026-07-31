package documenttree

import (
	"sort"
	"sync"
)

// Subdocuments returns the de-duplicated subdocument references reachable
// from currently visible roots. A reference is metadata only: callers must
// authenticate and authorize the separately negotiated subdocument manifest
// before opening a provider for it.
func (d *Document) Subdocuments() []SubdocumentRef {
	if d == nil {
		return nil
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	refs := make(map[string]SubdocumentRef)
	seen := make(map[ObjectID]struct{})
	var visitValue func(Value)
	var visitObject func(ObjectID)
	visitValue = func(value Value) {
		switch value.Kind {
		case ValueSubdocument:
			refs[value.Subdocument.ID] = value.Subdocument
		case ValueObject:
			visitObject(value.Object.ID)
		}
	}
	visitObject = func(id ObjectID) {
		if _, exists := seen[id]; exists {
			return
		}
		object, exists := d.state.objects[id]
		if !exists {
			return
		}
		seen[id] = struct{}{}
		switch object.kind {
		case KindMap:
			for _, entry := range d.state.maps[id] {
				if entry.present {
					visitValue(entry.value)
				}
			}
		case KindArray:
			for _, node := range d.visibleArrayNodesLocked(id) {
				visitValue(node.value)
			}
		}
	}
	for _, root := range d.state.roots {
		if d.isObjectKindLocked(root.id, root.kind) {
			visitObject(root.id)
		}
	}
	result := make([]SubdocumentRef, 0, len(refs))
	for _, ref := range refs {
		result = append(result, ref)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

// RegistryOptions bounds local, ephemeral subdocument lifecycle metadata.
// It is intentionally independent of Document.Options because opened content
// belongs to separately authorized replication groups.
type RegistryOptions struct {
	MaxSubdocuments int
	MaxIDBytes      int
}

func DefaultRegistryOptions() RegistryOptions {
	return RegistryOptions{MaxSubdocuments: 1 << 14, MaxIDBytes: 256}
}

func (o RegistryOptions) valid() bool { return o.MaxSubdocuments > 0 && o.MaxIDBytes > 0 }

// Registry tracks which visible subdocuments a local consumer has requested
// to load. Sync, Load, and Unload do not mutate the parent CRDT and do not
// perform network I/O; a provider uses Loaded to decide which independent
// manifest/group to fetch or release.
type Registry struct {
	mu      sync.RWMutex
	options RegistryOptions
	known   map[string]SubdocumentRef
	loaded  map[string]struct{}
}

func NewRegistry(options RegistryOptions) (*Registry, error) {
	if !options.valid() {
		return nil, ErrResourceLimit
	}
	return &Registry{options: options, known: make(map[string]SubdocumentRef), loaded: make(map[string]struct{})}, nil
}

// Sync refreshes the registry from one document's visible reference graph.
// It is atomic: an over-limit document leaves the prior local lifecycle state
// untouched.
func (r *Registry) Sync(document *Document) error {
	if r == nil {
		return ErrNilDocument
	}
	if document == nil {
		return ErrNilDocument
	}
	references := document.Subdocuments()
	if len(references) > r.options.MaxSubdocuments {
		return ErrResourceLimit
	}
	next := make(map[string]SubdocumentRef, len(references))
	for _, reference := range references {
		if !document.validName(reference.ID, r.options.MaxIDBytes) {
			return ErrInvalidValue
		}
		next[reference.ID] = reference
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	nextLoaded := make(map[string]struct{}, len(r.loaded))
	for id := range r.loaded {
		if _, exists := next[id]; exists {
			nextLoaded[id] = struct{}{}
		}
	}
	r.known, r.loaded = next, nextLoaded
	return nil
}

// Load records a local request to load one currently visible subdocument.
// Callers open their own provider after this returns true.
func (r *Registry) Load(id string) (SubdocumentRef, bool) {
	if r == nil {
		return SubdocumentRef{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	reference, exists := r.known[id]
	if !exists {
		return SubdocumentRef{}, false
	}
	r.loaded[id] = struct{}{}
	return reference, true
}

// Unload releases the local load request. It does not delete the parent
// reference and cannot revoke a remote peer's access.
func (r *Registry) Unload(id string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.loaded[id]; !exists {
		return false
	}
	delete(r.loaded, id)
	return true
}

// Available returns sorted visible references, regardless of local load
// state. The returned slice is caller-owned.
func (r *Registry) Available() []SubdocumentRef {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	result := make([]SubdocumentRef, 0, len(r.known))
	for _, reference := range r.known {
		result = append(result, reference)
	}
	r.mu.RUnlock()
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

// Loaded reports whether the reference is locally requested/opened.
func (r *Registry) Loaded(id string) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	_, loaded := r.loaded[id]
	r.mu.RUnlock()
	return loaded
}
