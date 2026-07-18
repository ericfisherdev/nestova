package adapter

import (
	"fmt"

	"github.com/ericfisherdev/nestova/internal/media/domain"
)

// StoreResolver is the map-backed domain.PhotoStoreResolver: a fixed set of
// PhotoStore instances, one per StorageBackend the composition root actually
// constructed (NES-132) — see that port's doc for why reads resolve by a
// row's own persisted backend rather than the deployment's current write
// target.
type StoreResolver struct {
	stores map[domain.StorageBackend]domain.PhotoStore
}

var _ domain.PhotoStoreResolver = (*StoreResolver)(nil)

// NewStoreResolver constructs a StoreResolver over stores, panicking if
// stores is empty or contains a nil entry, or an entry keyed by an invalid
// StorageBackend — every registered store must be genuinely usable, since
// Resolve hands the caller whatever is registered without a second check.
// stores is copied defensively so the caller's map cannot be mutated out
// from under the resolver after construction.
func NewStoreResolver(stores map[domain.StorageBackend]domain.PhotoStore) *StoreResolver {
	if len(stores) == 0 {
		panic("media/adapter: NewStoreResolver requires at least one store")
	}
	copied := make(map[domain.StorageBackend]domain.PhotoStore, len(stores))
	for backend, store := range stores {
		if !backend.Valid() {
			panic(fmt.Sprintf("media/adapter: NewStoreResolver received an invalid backend key %q", backend))
		}
		if store == nil {
			panic(fmt.Sprintf("media/adapter: NewStoreResolver received a nil store for backend %q", backend))
		}
		copied[backend] = store
	}
	return &StoreResolver{stores: copied}
}

// Resolve returns the PhotoStore registered for backend, or
// domain.ErrStoreNotConfigured when this deployment never constructed one
// for it.
func (r *StoreResolver) Resolve(backend domain.StorageBackend) (domain.PhotoStore, error) {
	store, ok := r.stores[backend]
	if !ok {
		return nil, fmt.Errorf("%w: %s", domain.ErrStoreNotConfigured, backend)
	}
	return store, nil
}
