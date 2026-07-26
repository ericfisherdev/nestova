package adapter_test

import (
	"context"
	"encoding/json"
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

func TestNestorageClientReadAccountsSuccess(t *testing.T) {
	hh := household.NewHouseholdID()
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("household")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accounts":[
			{"id":"remote-1","display_name":"Maya","email":"maya@example.com","active":true,"member_id":null},
			{"id":"remote-2","display_name":"Sam","email":"sam@example.com","active":true,"member_id":"member-x"}
		]}`))
	}))
	defer srv.Close()

	client := adapter.NewNestorageClient()
	accounts, err := client.ReadAccounts(context.Background(), srv.URL, "key", hh)
	if err != nil {
		t.Fatalf("ReadAccounts: %v", err)
	}
	if gotQuery != hh.String() {
		t.Errorf("household query param = %q, want %q", gotQuery, hh.String())
	}
	want := []domain.RemoteAccount{
		{RemoteUserID: "remote-1", Email: "maya@example.com", DisplayName: "Maya"},
		{RemoteUserID: "remote-2", Email: "sam@example.com", DisplayName: "Sam"},
	}
	if len(accounts) != len(want) {
		t.Fatalf("ReadAccounts() returned %d accounts, want %d", len(accounts), len(want))
	}
	for i, got := range accounts {
		if got != want[i] {
			t.Errorf("accounts[%d] = %+v, want %+v", i, got, want[i])
		}
	}
}

func TestNestorageClientReadAccountsInvalidAPIKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	client := adapter.NewNestorageClient()
	_, err := client.ReadAccounts(context.Background(), srv.URL, "wrong-key", household.NewHouseholdID())
	if !errors.Is(err, domain.ErrInvalidAPIKey) {
		t.Fatalf("ReadAccounts() error = %v, want ErrInvalidAPIKey", err)
	}
}

func TestNestorageClientReadAccountsUnexpectedStatusIsUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := adapter.NewNestorageClient()
	_, err := client.ReadAccounts(context.Background(), srv.URL, "key", household.NewHouseholdID())
	if !errors.Is(err, domain.ErrInstanceUnreachable) {
		t.Fatalf("ReadAccounts() error = %v, want ErrInstanceUnreachable", err)
	}
}

func TestNestorageClientProvisionLinkSendsExpectedBodyAndPath(t *testing.T) {
	hh := household.NewHouseholdID()
	member := household.NewMemberID()
	var (
		gotMethod string
		gotPath   string
		gotAuth   string
		gotBody   map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"remote-1","display_name":"Maya","email":"maya@example.com","active":true,"member_id":"` + member.String() + `"}`))
	}))
	defer srv.Close()

	client := adapter.NewNestorageClient()
	account, err := client.Provision(context.Background(), srv.URL, "key", hh, member, domain.ProvisionRequest{
		Link: &domain.ProvisionLink{RemoteUserID: "remote-1"},
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("method = %q, want PUT", gotMethod)
	}
	if gotPath != "/api/v1/federation/members/"+member.String() {
		t.Errorf("path = %q, want the NSTR-101 members path", gotPath)
	}
	if gotAuth != "Bearer key" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer key")
	}
	if gotBody["household_id"] != hh.String() {
		t.Errorf("body household_id = %v, want %q", gotBody["household_id"], hh.String())
	}
	link, _ := gotBody["link"].(map[string]any)
	if link["user_id"] != "remote-1" {
		t.Errorf("body link.user_id = %v, want %q", link["user_id"], "remote-1")
	}
	if _, hasAccount := gotBody["account"]; hasAccount {
		t.Errorf("body carries an account field for a Link request: %v", gotBody)
	}
	want := domain.RemoteAccount{RemoteUserID: "remote-1", Email: "maya@example.com", DisplayName: "Maya"}
	if account != want {
		t.Errorf("Provision() = %+v, want %+v", account, want)
	}
}

func TestNestorageClientProvisionCreateSendsExpectedBody(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"remote-new","display_name":"Sam","email":"sam@example.com","active":true,"member_id":null}`))
	}))
	defer srv.Close()

	client := adapter.NewNestorageClient()
	account, err := client.Provision(context.Background(), srv.URL, "key", household.NewHouseholdID(), household.NewMemberID(), domain.ProvisionRequest{
		Create: &domain.ProvisionCreate{DisplayName: "Sam", Email: "sam@example.com", Role: "member"},
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	account2, _ := gotBody["account"].(map[string]any)
	if account2["display_name"] != "Sam" || account2["email"] != "sam@example.com" || account2["role"] != "member" {
		t.Errorf("body account = %v, want display_name/email/role for Sam", account2)
	}
	if account2["active"] != true {
		t.Errorf("body account.active = %v, want true", account2["active"])
	}
	if _, hasLink := gotBody["link"]; hasLink {
		t.Errorf("body carries a link field for a Create request: %v", gotBody)
	}
	want := domain.RemoteAccount{RemoteUserID: "remote-new", Email: "sam@example.com", DisplayName: "Sam"}
	if account != want {
		t.Errorf("Provision() = %+v, want %+v", account, want)
	}
}

func TestNestorageClientProvisionRequiresExactlyOneVariant(t *testing.T) {
	client := adapter.NewNestorageClient()
	tests := []struct {
		name string
		req  domain.ProvisionRequest
	}{
		{name: "neither set", req: domain.ProvisionRequest{}},
		{
			name: "both set",
			req: domain.ProvisionRequest{
				Link:   &domain.ProvisionLink{RemoteUserID: "remote-1"},
				Create: &domain.ProvisionCreate{DisplayName: "Sam", Email: "sam@example.com", Role: "member"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := client.Provision(context.Background(), "https://nestorage.example.ts.net", "key", household.NewHouseholdID(), household.NewMemberID(), tt.req)
			if err == nil {
				t.Fatal("Provision() error = nil, want error")
			}
		})
	}
}

func TestNestorageClientProvisionConflict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	defer srv.Close()

	client := adapter.NewNestorageClient()
	_, err := client.Provision(context.Background(), srv.URL, "key", household.NewHouseholdID(), household.NewMemberID(), domain.ProvisionRequest{
		Link: &domain.ProvisionLink{RemoteUserID: "remote-1"},
	})
	if !errors.Is(err, domain.ErrLinkConflict) {
		t.Fatalf("Provision() error = %v, want ErrLinkConflict", err)
	}
}

func TestNestorageClientProvisionInvalidAPIKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	client := adapter.NewNestorageClient()
	_, err := client.Provision(context.Background(), srv.URL, "key", household.NewHouseholdID(), household.NewMemberID(), domain.ProvisionRequest{
		Link: &domain.ProvisionLink{RemoteUserID: "remote-1"},
	})
	if !errors.Is(err, domain.ErrInvalidAPIKey) {
		t.Fatalf("Provision() error = %v, want ErrInvalidAPIKey", err)
	}
}

func TestNestorageClientProvisionUnexpectedStatusIsUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := adapter.NewNestorageClient()
	_, err := client.Provision(context.Background(), srv.URL, "key", household.NewHouseholdID(), household.NewMemberID(), domain.ProvisionRequest{
		Link: &domain.ProvisionLink{RemoteUserID: "remote-1"},
	})
	if !errors.Is(err, domain.ErrInstanceUnreachable) {
		t.Fatalf("Provision() error = %v, want ErrInstanceUnreachable", err)
	}
}
