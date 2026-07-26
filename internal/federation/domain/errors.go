package domain

import "errors"

// InstanceLinkRepository domain errors.
var (
	// ErrLinkNotFound is returned by GetByHousehold/GetByBaseURL when no
	// federation instance link matches.
	ErrLinkNotFound = errors.New("federation: instance link not found")
	// ErrHouseholdAlreadyBound is returned by Create when the household
	// already has a linked Nestorage instance — the storage-level backstop
	// is federation_instance_link's UNIQUE(household_id) constraint
	// (migration 00038); LinkService.Attach's own pre-check exists only to
	// give a friendlier error before that constraint is ever reached.
	ErrHouseholdAlreadyBound = errors.New("federation: household is already bound to a nestorage instance")
	// ErrInstanceAlreadyBound is returned by Create when the (normalized)
	// base url is already linked to a DIFFERENT household — the
	// storage-level backstop is the unique index on lower(base_url)
	// (migration 00038).
	ErrInstanceAlreadyBound = errors.New("federation: nestorage instance is already bound to another household")
)

// ErrInvalidBaseURL is returned by NormalizeBaseURL and InstanceLink.Validate
// when the supplied Nestorage base url is not an absolute http(s) url.
var ErrInvalidBaseURL = errors.New("federation: base url must be an absolute http(s) url")

// instanceVerifier (adapter.NestorageClient) errors. Both are returned
// directly by Verify's own status-code mapping — never constructed
// elsewhere — so LinkService.Attach can propagate whichever one occurred
// straight through to the settings UI, which shows a different message for
// each (a wrong/revoked key vs. an unreachable or misconfigured instance).
var (
	// ErrInvalidAPIKey is returned when the Nestorage instance rejects the
	// presented api key (401) or its scope (403) — distinguishing this from
	// ErrInstanceUnreachable is exactly the AC 2 requirement that "a wrong
	// key or unreachable instance produces a clear error".
	ErrInvalidAPIKey = errors.New("federation: nestorage instance rejected the api key")
	// ErrInstanceUnreachable is returned for any connection failure,
	// timeout, or unexpected status the instance responds with — anything
	// that is not conclusively an api-key rejection.
	ErrInstanceUnreachable = errors.New("federation: nestorage instance is unreachable")
)
