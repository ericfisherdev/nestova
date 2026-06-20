package adapter

import (
	"context"
	"net/http"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// googleCalendarReadonlyScope grants read-only access to a user's calendars. It
// is the literal value of calendar.CalendarReadonlyScope; defined here so the
// OAuth ticket does not pull the full Google Calendar API client (NES-68 adds
// that dependency for the sync engine).
const googleCalendarReadonlyScope = "https://www.googleapis.com/auth/calendar.readonly"

// oauthHTTPTimeout bounds every outbound call to Google's token endpoint so a
// slow or hung response cannot stall a request or a sync tick.
const oauthHTTPTimeout = 15 * time.Second

// GoogleOAuthClient wraps an oauth2.Config for the Google authorization-code
// flow. Credentials are injected (from config) and never logged.
type GoogleOAuthClient struct {
	config     *oauth2.Config
	httpClient *http.Client
}

// NewGoogleOAuthClient builds the client from the configured credentials and
// redirect URL, requesting read-only calendar access.
func NewGoogleOAuthClient(clientID, clientSecret, redirectURL string) *GoogleOAuthClient {
	return &GoogleOAuthClient{
		config: &oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			RedirectURL:  redirectURL,
			Endpoint:     google.Endpoint,
			Scopes:       []string{googleCalendarReadonlyScope},
		},
		httpClient: &http.Client{Timeout: oauthHTTPTimeout},
	}
}

// AuthCodeURL builds the Google consent-screen URL for the signed state.
// AccessTypeOffline plus ApprovalForce request a refresh token even on a repeat
// authorization, which the connect flow needs to refresh access tokens later.
func (c *GoogleOAuthClient) AuthCodeURL(state string) string {
	return c.config.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.ApprovalForce)
}

// Exchange trades an authorization code for a token, using the bounded HTTP
// client so the token-endpoint call cannot hang.
func (c *GoogleOAuthClient) Exchange(ctx context.Context, code string) (*oauth2.Token, error) {
	return c.config.Exchange(c.withClient(ctx), code)
}

// TokenSource returns a self-refreshing token source seeded with tok; it
// refreshes the access token via the stored refresh token when it expires.
func (c *GoogleOAuthClient) TokenSource(ctx context.Context, tok *oauth2.Token) oauth2.TokenSource {
	return c.config.TokenSource(c.withClient(ctx), tok)
}

// withClient binds the bounded HTTP client to ctx so oauth2 uses it for the
// token-endpoint request.
func (c *GoogleOAuthClient) withClient(ctx context.Context) context.Context {
	return context.WithValue(ctx, oauth2.HTTPClient, c.httpClient)
}
