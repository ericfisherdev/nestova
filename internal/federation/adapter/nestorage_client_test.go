package adapter_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ericfisherdev/nestova/internal/federation/adapter"
	"github.com/ericfisherdev/nestova/internal/federation/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

func TestNestorageClientVerifySuccess(t *testing.T) {
	hh := household.NewHouseholdID()
	var (
		gotAuth  string
		gotPath  string
		gotQuery string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotQuery = r.URL.Query().Get("household")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := adapter.NewNestorageClient()
	if err := client.Verify(context.Background(), srv.URL, "nstr_test_key", hh); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if gotAuth != "Bearer nstr_test_key" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer nstr_test_key")
	}
	if gotPath != "/api/v1/federation/accounts" {
		t.Errorf("request path = %q, want the NSTR-101 accounts path", gotPath)
	}
	if gotQuery != hh.String() {
		t.Errorf("household query param = %q, want %q", gotQuery, hh.String())
	}
}

func TestNestorageClientVerifyInvalidAPIKey(t *testing.T) {
	tests := []struct {
		name   string
		status int
	}{
		{name: "401 unauthorized", status: http.StatusUnauthorized},
		{name: "403 forbidden", status: http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()

			client := adapter.NewNestorageClient()
			err := client.Verify(context.Background(), srv.URL, "wrong-key", household.NewHouseholdID())
			if !errors.Is(err, domain.ErrInvalidAPIKey) {
				t.Fatalf("Verify() error = %v, want ErrInvalidAPIKey", err)
			}
		})
	}
}

func TestNestorageClientVerifyUnexpectedStatusIsUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := adapter.NewNestorageClient()
	err := client.Verify(context.Background(), srv.URL, "key", household.NewHouseholdID())
	if !errors.Is(err, domain.ErrInstanceUnreachable) {
		t.Fatalf("Verify() error = %v, want ErrInstanceUnreachable", err)
	}
}

func TestNestorageClientVerifyConnectionFailureIsUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	// Close before calling Verify so the connection is refused — the
	// http.Client error path, distinct from every status-code branch above.
	srv.Close()

	client := adapter.NewNestorageClient()
	err := client.Verify(context.Background(), srv.URL, "key", household.NewHouseholdID())
	if !errors.Is(err, domain.ErrInstanceUnreachable) {
		t.Fatalf("Verify() error = %v, want ErrInstanceUnreachable", err)
	}
}

func TestNestorageClientVerifyNeverLogsAPIKeyInError(t *testing.T) {
	// A malformed base url fails at request-construction time, the one
	// error path that formats details into the returned error itself
	// (every other branch returns a bare sentinel) — confirming the key
	// never leaks into that message.
	client := adapter.NewNestorageClient()
	err := client.Verify(context.Background(), "://not-a-url", "super-secret-key", household.NewHouseholdID())
	if err == nil {
		t.Fatal("Verify() error = nil, want error for a malformed base url")
	}
	if got := err.Error(); strings.Contains(got, "super-secret-key") {
		t.Errorf("error message contains the raw api key: %s", got)
	}
}
