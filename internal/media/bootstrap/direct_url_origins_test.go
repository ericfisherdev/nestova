package bootstrap_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/ericfisherdev/nestova/internal/media/bootstrap"
	mediadomain "github.com/ericfisherdev/nestova/internal/media/domain"
)

type fakeResolver struct {
	store mediadomain.PhotoStore
	err   error
}

func (r fakeResolver) Resolve(mediadomain.StorageBackend) (mediadomain.PhotoStore, error) {
	return r.store, r.err
}

// plainStore is a PhotoStore that does not redirect: it has no origin to
// report. The embedded nil interface is never called.
type plainStore struct{ mediadomain.PhotoStore }

type redirectingStore struct {
	mediadomain.PhotoStore
	origin string
	err    error
}

func (s redirectingStore) DirectURLOrigin(context.Context) (string, error) { return s.origin, s.err }

func TestDirectURLOrigins(t *testing.T) {
	boom := errors.New("cannot sign")
	cases := []struct {
		name     string
		resolver fakeResolver
		want     []string
		wantErr  error
	}{
		{"no s3 store means no origins", fakeResolver{err: mediadomain.ErrStoreNotConfigured}, nil, nil},
		{"a store that does not redirect adds none", fakeResolver{store: plainStore{}}, nil, nil},
		{
			"a redirecting store contributes its origin",
			fakeResolver{store: redirectingStore{origin: "https://s3.example.com"}},
			[]string{"https://s3.example.com"},
			nil,
		},
		{"an origin failure is returned", fakeResolver{store: redirectingStore{err: boom}}, nil, boom},
		{"an unexpected resolve failure is returned", fakeResolver{err: boom}, nil, boom},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := bootstrap.DirectURLOrigins(context.Background(), tc.resolver)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("origins = %v, want %v", got, tc.want)
			}
		})
	}
}
