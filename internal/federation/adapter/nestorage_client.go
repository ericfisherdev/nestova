package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/ericfisherdev/nestova/internal/federation/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// nestorageVerifyTimeout bounds the attach flow's live verification call so
// a slow or hung Nestorage instance cannot stall a settings-page request,
// mirroring calendar/adapter.GoogleOAuthClient's own oauthHTTPTimeout. The
// same timeout also bounds ReadAccounts and Provision (NSTR-107): every
// call this client makes shares one bound, not a per-method one.
const nestorageVerifyTimeout = 10 * time.Second

// maxNestorageResponseBytes caps a Nestorage response body this client will
// read, so a misbehaving or compromised instance cannot exhaust memory with
// an unbounded payload — mirrors meals/adapter.ExternalRecipeSource's own
// maxResponseBytes rationale, sized to match Nestorage's own
// maxFederationRequestBytes (federation_api.go) for symmetry.
const maxNestorageResponseBytes = 1 << 20 // 1 MiB

// nestorageAccountsPath is NSTR-101's account-read endpoint: Verify's own
// reachability check, and ReadAccounts' (NSTR-107) full account listing —
// the same call that, per NSTR-101, records the server-side household
// binding on Nestorage's own first authenticated request carrying that
// household id.
const nestorageAccountsPath = "/api/v1/federation/accounts"

// nestorageMembersPathPrefix is NSTR-101's provision endpoint
// (PUT {prefix}{member_id}) that Provision (NSTR-107/108) calls to link a
// member to an existing Nestorage account or create a new one.
const nestorageMembersPathPrefix = "/api/v1/federation/members/"

// NestorageClient calls a Nestorage instance's federation API to verify an
// attach credential (NSTR-106). Its own http.Client carries a bounded
// timeout, matching GoogleOAuthClient's convention; no request or response
// here ever logs the presented api key.
type NestorageClient struct {
	httpClient *http.Client
}

// NewNestorageClient constructs the client with a bounded-timeout
// http.Client.
func NewNestorageClient() *NestorageClient {
	return &NestorageClient{httpClient: &http.Client{Timeout: nestorageVerifyTimeout}}
}

// Verify calls GET {baseURL}/api/v1/federation/accounts?household={householdID}
// with apiKey presented as a bearer credential. A 200 response proves
// reachability, key validity, and the widened household scope in one call.
// Returns domain.ErrInvalidAPIKey for a 401/403 response (the instance is
// reachable but rejects the credential), or domain.ErrInstanceUnreachable
// for a connection failure, timeout, or any other status.
func (c *NestorageClient) Verify(ctx context.Context, baseURL, apiKey string, householdID household.HouseholdID) error {
	path := nestorageAccountsPath + "?household=" + url.QueryEscape(householdID.String())
	req, err := newAuthorizedRequest(ctx, http.MethodGet, baseURL, path, apiKey, nil)
	if err != nil {
		return err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", domain.ErrInstanceUnreachable, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return domain.ErrInvalidAPIKey
	default:
		return fmt.Errorf("%w: unexpected status %d", domain.ErrInstanceUnreachable, resp.StatusCode)
	}
}

// accountsResponse is GET {nestorageAccountsPath}'s response envelope —
// matches NSTR-101's federationAccountsResponse (federation_api.go).
type accountsResponse struct {
	Accounts []accountResponse `json:"accounts"`
}

// accountResponse is one federation account row — matches NSTR-101's
// federationAccountResponse. Active and MemberID are decoded but unused
// here: reconciliation's own existing-link state comes from this
// context's own MemberLinkRepository, never the remote server's opinion of
// it.
type accountResponse struct {
	ID          string  `json:"id"`
	DisplayName string  `json:"display_name"`
	Email       string  `json:"email"`
	Active      bool    `json:"active"`
	MemberID    *string `json:"member_id"`
}

// ReadAccounts calls GET {baseURL}/api/v1/federation/accounts?household={householdID}
// and returns every account Nestorage reports, projected onto
// domain.RemoteAccount — enough for domain.ProposeMatches and nothing
// more. Returns domain.ErrInvalidAPIKey for a 401/403 response, or
// domain.ErrInstanceUnreachable for a connection failure, timeout, or any
// other status.
func (c *NestorageClient) ReadAccounts(ctx context.Context, baseURL, apiKey string, householdID household.HouseholdID) ([]domain.RemoteAccount, error) {
	path := nestorageAccountsPath + "?household=" + url.QueryEscape(householdID.String())
	req, err := newAuthorizedRequest(ctx, http.MethodGet, baseURL, path, apiKey, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrInstanceUnreachable, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		var decoded accountsResponse
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxNestorageResponseBytes)).Decode(&decoded); err != nil {
			return nil, fmt.Errorf("federation/adapter: decode accounts response: %w", err)
		}
		accounts := make([]domain.RemoteAccount, 0, len(decoded.Accounts))
		for _, a := range decoded.Accounts {
			accounts = append(accounts, domain.RemoteAccount{RemoteUserID: a.ID, Email: a.Email, DisplayName: a.DisplayName})
		}
		return accounts, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, domain.ErrInvalidAPIKey
	default:
		return nil, fmt.Errorf("%w: unexpected status %d", domain.ErrInstanceUnreachable, resp.StatusCode)
	}
}

// provisionRequestBody mirrors NSTR-101's PUT
// /api/v1/federation/members/{member_id} body: household_id plus exactly
// one of link or account.
type provisionRequestBody struct {
	HouseholdID string                `json:"household_id"`
	Link        *provisionLinkBody    `json:"link,omitempty"`
	Account     *provisionAccountBody `json:"account,omitempty"`
}

// provisionLinkBody is provisionRequestBody's "link" variant: pair the
// path's member with an already-existing Nestorage user.
type provisionLinkBody struct {
	UserID string `json:"user_id"`
}

// provisionAccountBody is provisionRequestBody's "account" variant: the
// profile fields to create or update a federated user from.
type provisionAccountBody struct {
	DisplayName string `json:"display_name"`
	Email       string `json:"email"`
	Role        string `json:"role"`
	// Active is always true: NSTR-107 never provisions a deactivated
	// account — deactivating one is a Nestorage-side admin action, out of
	// scope for reconciliation.
	Active bool `json:"active"`
}

// Provision calls PUT {baseURL}/api/v1/federation/members/{memberID} with
// exactly one of req.Link or req.Create, and returns the account NSTR-101's
// response describes: for Link, the confirmed existing account; for
// Create, the brand new (or updated) account it just provisioned. The PUT
// verb is what makes a repeated call with the same memberID and req an
// idempotent replay (200) rather than a duplicate (201 only on first
// creation) — the property NSTR-108's own re-push relies on. Returns
// domain.ErrInvalidAPIKey for a 401/403 response, domain.ErrLinkConflict
// for a 409 (the target is already linked to someone else), or
// domain.ErrInstanceUnreachable for a connection failure, timeout, or any
// other status.
func (c *NestorageClient) Provision(ctx context.Context, baseURL, apiKey string, householdID household.HouseholdID, memberID household.MemberID, req domain.ProvisionRequest) (domain.RemoteAccount, error) {
	body, err := provisionBody(householdID, req)
	if err != nil {
		return domain.RemoteAccount{}, err
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return domain.RemoteAccount{}, fmt.Errorf("federation/adapter: encode provision request: %w", err)
	}

	path := nestorageMembersPathPrefix + url.PathEscape(memberID.String())
	httpReq, err := newAuthorizedRequest(ctx, http.MethodPut, baseURL, path, apiKey, bytes.NewReader(payload))
	if err != nil {
		return domain.RemoteAccount{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return domain.RemoteAccount{}, fmt.Errorf("%w: %v", domain.ErrInstanceUnreachable, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		var decoded accountResponse
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxNestorageResponseBytes)).Decode(&decoded); err != nil {
			return domain.RemoteAccount{}, fmt.Errorf("federation/adapter: decode provision response: %w", err)
		}
		return domain.RemoteAccount{RemoteUserID: decoded.ID, Email: decoded.Email, DisplayName: decoded.DisplayName}, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return domain.RemoteAccount{}, domain.ErrInvalidAPIKey
	case http.StatusConflict:
		return domain.RemoteAccount{}, domain.ErrLinkConflict
	default:
		return domain.RemoteAccount{}, fmt.Errorf("%w: unexpected status %d", domain.ErrInstanceUnreachable, resp.StatusCode)
	}
}

// provisionBody validates that req carries exactly one of Link or Create —
// matching NSTR-101's own "both, or neither, answers 422" contract
// client-side, before any request is sent — and builds the wire body.
func provisionBody(householdID household.HouseholdID, req domain.ProvisionRequest) (provisionRequestBody, error) {
	if (req.Link == nil) == (req.Create == nil) {
		return provisionRequestBody{}, errors.New("federation/adapter: provision request must set exactly one of Link or Create")
	}
	body := provisionRequestBody{HouseholdID: householdID.String()}
	if req.Link != nil {
		body.Link = &provisionLinkBody{UserID: req.Link.RemoteUserID}
		return body, nil
	}
	body.Account = &provisionAccountBody{
		DisplayName: req.Create.DisplayName,
		Email:       req.Create.Email,
		Role:        req.Create.Role,
		Active:      true,
	}
	return body, nil
}

// newAuthorizedRequest builds an HTTP request against baseURL+path with
// apiKey presented as a bearer credential — the one place on this client
// that sets that header, so every method (Verify, ReadAccounts, Provision)
// shares it rather than risking the key being formatted differently, or
// accidentally logged, in some future new method.
func newAuthorizedRequest(ctx context.Context, method, baseURL, path, apiKey string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %v", domain.ErrInstanceUnreachable, err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	return req, nil
}
