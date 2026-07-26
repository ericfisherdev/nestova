package adapter

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/ericfisherdev/nestova/internal/federation/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// nestorageVerifyTimeout bounds the attach flow's live verification call so
// a slow or hung Nestorage instance cannot stall a settings-page request,
// mirroring calendar/adapter.GoogleOAuthClient's own oauthHTTPTimeout.
const nestorageVerifyTimeout = 10 * time.Second

// nestorageAccountsPath is NSTR-101's account-read endpoint this client
// verifies against — the same call that, per NSTR-101, records the
// server-side household binding on Nestorage's own first authenticated
// request carrying that household id.
const nestorageAccountsPath = "/api/v1/federation/accounts"

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
	reqURL := baseURL + nestorageAccountsPath + "?household=" + url.QueryEscape(householdID.String())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return fmt.Errorf("%w: build request: %v", domain.ErrInstanceUnreachable, err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

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
