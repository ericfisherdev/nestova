// Package app holds the federation bounded context's application services:
// LinkService, which attaches, detaches, and reads a household's Nestorage
// instance link (NSTR-106); and ReconciliationService, which proposes and
// confirms the member-to-account links that ride on top of it (NSTR-107).
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ericfisherdev/nestova/internal/federation/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// Nestorage's own two-way account.role values (NSTR-101's
// federationAccountRequest.Role) — nestorageRole's own mapping targets,
// distinct from nestova's three-way household.Role.
const (
	nestorageRoleAdmin  = "admin"
	nestorageRoleMember = "member"
)

// ErrNotAttached is returned by Proposals and Confirm when householdID has
// no Nestorage instance link (NSTR-106) — reconciliation has nothing to
// read or write against without one.
var ErrNotAttached = errors.New("federation: household is not attached to a nestorage instance")

// ErrMemberOutsideHousehold is returned by Confirm when a decision names a
// member who does not belong to householdID — accepting it would let one
// household's reconciliation confirm a link for a member outside the
// binding it was verified against.
var ErrMemberOutsideHousehold = errors.New("federation: decision names a member outside the household")

// credentialReader is the narrow read port (ISP) ReconciliationService
// needs onto LinkService — just Credential, mirroring
// internal/notify/app/household_reader.go's own narrow-port precedent:
// neither Attach, Detach, nor Status is this service's concern.
type credentialReader interface {
	Credential(ctx context.Context, householdID household.HouseholdID) (baseURL, apiKey string, err error)
}

// accountReader is the narrow read port (ISP) onto a NestorageClient's
// ReadAccounts, satisfied by *adapter.NestorageClient and faked in tests.
type accountReader interface {
	ReadAccounts(ctx context.Context, baseURL, apiKey string, householdID household.HouseholdID) ([]domain.RemoteAccount, error)
}

// provisioner is the narrow write port (ISP) onto a NestorageClient's
// Provision, satisfied by *adapter.NestorageClient and faked in tests.
type provisioner interface {
	Provision(ctx context.Context, baseURL, apiKey string, householdID household.HouseholdID, memberID household.MemberID, req domain.ProvisionRequest) (domain.RemoteAccount, error)
}

// memberReader is the narrow read port (ISP) onto a household's members,
// satisfied by *adapter.MemberReader and faked in tests.
type memberReader interface {
	MembersByHousehold(ctx context.Context, householdID household.HouseholdID) ([]domain.MemberCandidate, error)
}

// Proposal is Proposals' own per-member row: a household member joined
// with either the fact it is already linked, a proposed match, or an
// explicit no-match.
type Proposal struct {
	MemberID    household.MemberID
	DisplayName string
	// Linked is true when the member already has a MemberLink — Proposed
	// and Reason are always zero in that case; the confirm screen (NSTR-109)
	// shows LinkedRemoteUserID instead.
	Linked             bool
	LinkedRemoteUserID string
	// Proposed is the matched remote account domain.ProposeMatches found,
	// non-nil only when Linked is false and a unique email match exists.
	Proposed *domain.RemoteAccount
	Reason   domain.LinkOrigin
}

// Decision is one member's confirmed reconciliation choice, as Confirm
// consumes it: RemoteUserID set links to that existing account (an
// accepted proposal, or a human's own manual pairing); RemoteUserID nil
// provisions a brand new account for a confirmed no-match.
type Decision struct {
	MemberID     household.MemberID
	RemoteUserID *string
	// Manual marks a RemoteUserID decision the confirm screen's own human
	// paired by hand rather than accepting domain.ProposeMatches' own
	// proposal — the difference between MemberLink.LinkedVia recording
	// LinkOriginEmail and LinkOriginManual. Ignored when RemoteUserID is
	// nil (a create is always LinkOriginCreated).
	Manual bool
}

// Outcome is Confirm's own per-member result. It is never a bare error:
// collecting per-member failures here — rather than failing the whole
// batch on the first one — is what makes a partial failure retry-safe: the
// successes already recorded in MemberLinkRepository stay recorded, and a
// retried Confirm call (see Confirm's own doc) provisions only the
// failures.
type Outcome struct {
	Succeeded []household.MemberID
	// Failed is documented to carry domain.ErrLinkConflict (passed straight
	// through from a provision or Put race) among its values; every other
	// value is a wrapped provisioning error.
	Failed map[household.MemberID]error
}

// ReconciliationService computes and confirms the member-to-account links
// riding on a household's Nestorage instance link (NSTR-106). A Proposal is
// always a suggestion, never a decision: Proposals performs no writes,
// local or remote — only Confirm does, and only for the decisions it is
// given.
type ReconciliationService struct {
	binding   credentialReader
	accounts  accountReader
	provision provisioner
	links     domain.MemberLinkRepository
	members   memberReader
	logger    *slog.Logger
}

// NewReconciliationService constructs the service with injected
// dependencies, returning an error on any nil dependency (mirrors
// NewLinkService's own constructor-injection convention).
func NewReconciliationService(binding credentialReader, accounts accountReader, provision provisioner, links domain.MemberLinkRepository, members memberReader, logger *slog.Logger) (*ReconciliationService, error) {
	if binding == nil {
		return nil, errors.New("federation: NewReconciliationService requires a non-nil binding reader")
	}
	if accounts == nil {
		return nil, errors.New("federation: NewReconciliationService requires a non-nil account reader")
	}
	if provision == nil {
		return nil, errors.New("federation: NewReconciliationService requires a non-nil provisioner")
	}
	if links == nil {
		return nil, errors.New("federation: NewReconciliationService requires a non-nil link repository")
	}
	if members == nil {
		return nil, errors.New("federation: NewReconciliationService requires a non-nil member reader")
	}
	if logger == nil {
		return nil, errors.New("federation: NewReconciliationService requires a non-nil logger")
	}
	return &ReconciliationService{binding: binding, accounts: accounts, provision: provision, links: links, members: members, logger: logger}, nil
}

// Proposals returns one row per member of householdID, each carrying
// either a proposed Nestorage account with its match reason, an explicit
// no-match, or (for a member with an existing MemberLink) the fact it is
// already linked. It performs no writes, local or remote. Returns
// ErrNotAttached when householdID has no Nestorage instance link.
func (s *ReconciliationService) Proposals(ctx context.Context, householdID household.HouseholdID) ([]Proposal, error) {
	baseURL, apiKey, err := s.credential(ctx, householdID)
	if err != nil {
		return nil, err
	}

	members, err := s.members.MembersByHousehold(ctx, householdID)
	if err != nil {
		return nil, fmt.Errorf("reconciliation: read members: %w", err)
	}
	remoteAccounts, err := s.accounts.ReadAccounts(ctx, baseURL, apiKey, householdID)
	if err != nil {
		return nil, fmt.Errorf("reconciliation: read remote accounts: %w", err)
	}
	existingLinks, err := s.links.ListByHousehold(ctx, householdID)
	if err != nil {
		return nil, fmt.Errorf("reconciliation: read existing links: %w", err)
	}

	linkedByMember := make(map[household.MemberID]domain.MemberLink, len(existingLinks))
	for _, l := range existingLinks {
		linkedByMember[l.MemberID] = l
	}
	remoteByID := make(map[string]domain.RemoteAccount, len(remoteAccounts))
	for _, a := range remoteAccounts {
		remoteByID[a.RemoteUserID] = a
	}
	proposalsByMember := make(map[household.MemberID]domain.MatchProposal, len(members))
	for _, p := range domain.ProposeMatches(members, remoteAccounts, existingLinks) {
		proposalsByMember[p.MemberID] = p
	}

	rows := make([]Proposal, 0, len(members))
	for _, m := range members {
		rows = append(rows, proposalRow(m, linkedByMember, proposalsByMember, remoteByID))
	}
	return rows, nil
}

// proposalRow builds one member's own Proposal row from the already-loaded
// link/proposal/account indexes Proposals assembled.
func proposalRow(m domain.MemberCandidate, linkedByMember map[household.MemberID]domain.MemberLink, proposalsByMember map[household.MemberID]domain.MatchProposal, remoteByID map[string]domain.RemoteAccount) Proposal {
	row := Proposal{MemberID: m.MemberID, DisplayName: m.DisplayName}
	if link, ok := linkedByMember[m.MemberID]; ok {
		row.Linked = true
		row.LinkedRemoteUserID = link.RemoteUserID
		return row
	}
	proposal, ok := proposalsByMember[m.MemberID]
	if !ok || proposal.RemoteUserID == nil {
		return row
	}
	if account, found := remoteByID[*proposal.RemoteUserID]; found {
		row.Proposed = &account
		row.Reason = proposal.MatchReason
	}
	return row
}

// Confirm executes decisions against householdID's bound Nestorage
// instance: for each decision not already linked, it calls Provision (link
// for an accepted or manually chosen match, create for a confirmed
// no-match) and records the resulting link before moving to the next one —
// so a mid-batch failure never loses completed work, and a retried Confirm
// call with the same decisions re-provisions only the ones that failed
// (the successes are already-linked members on the retry, and are silently
// skipped exactly like any other already-linked member). Confirm never
// fails fast on a provisioning error; every such failure is collected into
// the returned Outcome instead.
//
// Returns ErrNotAttached when householdID has no Nestorage instance link,
// or ErrMemberOutsideHousehold when any decision names a member who does
// not belong to householdID — both checked, and both failing the WHOLE
// call, before any decision is processed. domain.ErrLinkConflict can appear
// as one of Outcome.Failed's values, passed through unchanged from a
// provision or MemberLinkRepository.Put race.
func (s *ReconciliationService) Confirm(ctx context.Context, householdID household.HouseholdID, decisions []Decision) (Outcome, error) {
	baseURL, apiKey, err := s.credential(ctx, householdID)
	if err != nil {
		return Outcome{}, err
	}

	members, err := s.members.MembersByHousehold(ctx, householdID)
	if err != nil {
		return Outcome{}, fmt.Errorf("reconciliation: read members: %w", err)
	}
	memberByID := make(map[household.MemberID]domain.MemberCandidate, len(members))
	for _, m := range members {
		memberByID[m.MemberID] = m
	}
	for _, d := range decisions {
		if _, ok := memberByID[d.MemberID]; !ok {
			return Outcome{}, fmt.Errorf("%w: %s", ErrMemberOutsideHousehold, d.MemberID)
		}
	}

	existingLinks, err := s.links.ListByHousehold(ctx, householdID)
	if err != nil {
		return Outcome{}, fmt.Errorf("reconciliation: read existing links: %w", err)
	}
	alreadyLinked := make(map[household.MemberID]bool, len(existingLinks))
	for _, l := range existingLinks {
		alreadyLinked[l.MemberID] = true
	}

	outcome := Outcome{Failed: make(map[household.MemberID]error)}
	for _, d := range decisions {
		if alreadyLinked[d.MemberID] {
			// Re-running reconciliation must skip an already-linked member
			// rather than provision (and potentially create) a duplicate
			// account — this is what makes a retry after a partial
			// failure safe.
			continue
		}
		if err := s.confirmOne(ctx, baseURL, apiKey, householdID, memberByID[d.MemberID], d); err != nil {
			outcome.Failed[d.MemberID] = err
			continue
		}
		outcome.Succeeded = append(outcome.Succeeded, d.MemberID)
	}
	return outcome, nil
}

// confirmOne provisions and records the link for a single decision,
// mapping decision+member into the request Provision needs, then recording
// the result via s.links.Put BEFORE returning — so this member's success
// survives even if a later decision in the same batch fails.
func (s *ReconciliationService) confirmOne(ctx context.Context, baseURL, apiKey string, householdID household.HouseholdID, member domain.MemberCandidate, decision Decision) error {
	req, via := provisionRequestFor(member, decision)

	account, err := s.provision.Provision(ctx, baseURL, apiKey, householdID, member.MemberID, req)
	if err != nil {
		return fmt.Errorf("provision member: %w", err)
	}

	link := domain.MemberLink{
		MemberID:     member.MemberID,
		HouseholdID:  householdID,
		RemoteUserID: account.RemoteUserID,
		LinkedVia:    via,
		LinkedAt:     time.Now(),
	}
	if err := s.links.Put(ctx, link); err != nil {
		return fmt.Errorf("record link: %w", err)
	}
	s.logger.InfoContext(ctx, "nestorage member link recorded",
		"household_id", householdID.String(), "member_id", member.MemberID.String(), "linked_via", via.String())
	return nil
}

// provisionRequestFor builds decision's own domain.ProvisionRequest and the
// domain.LinkOrigin the resulting link should record: RemoteUserID set
// links to that account (LinkOriginManual when decision.Manual, otherwise
// the accepted-proposal default LinkOriginEmail); RemoteUserID nil creates
// a brand new account from member's own profile (always LinkOriginCreated).
func provisionRequestFor(member domain.MemberCandidate, decision Decision) (domain.ProvisionRequest, domain.LinkOrigin) {
	if decision.RemoteUserID != nil {
		via := domain.LinkOriginEmail
		if decision.Manual {
			via = domain.LinkOriginManual
		}
		return domain.ProvisionRequest{Link: &domain.ProvisionLink{RemoteUserID: *decision.RemoteUserID}}, via
	}

	email := ""
	if member.Email != nil {
		email = *member.Email
	}
	create := domain.ProvisionRequest{Create: &domain.ProvisionCreate{
		DisplayName: member.DisplayName,
		Email:       email,
		Role:        nestorageRole(member.Role),
	}}
	return create, domain.LinkOriginCreated
}

// nestorageRole maps nestova's own three-way household.Role to Nestorage's
// two-way admin/member account role (NSTR-101's account.role): a
// parent-privileged member (owner or adult) provisions as "admin", a child
// as "member" — the same distinction household.Role.IsParent() already
// draws for every other bounded context that needs it.
func nestorageRole(r household.Role) string {
	if r.IsParent() {
		return nestorageRoleAdmin
	}
	return nestorageRoleMember
}

// credential fetches householdID's Nestorage credential, mapping
// domain.ErrLinkNotFound (LinkService's own "unbound" sentinel) to this
// service's own ErrNotAttached — Proposals and Confirm share this single
// translation point rather than each re-checking errors.Is themselves.
func (s *ReconciliationService) credential(ctx context.Context, householdID household.HouseholdID) (baseURL, apiKey string, err error) {
	baseURL, apiKey, err = s.binding.Credential(ctx, householdID)
	if err != nil {
		if errors.Is(err, domain.ErrLinkNotFound) {
			return "", "", ErrNotAttached
		}
		return "", "", fmt.Errorf("reconciliation: read binding: %w", err)
	}
	return baseURL, apiKey, nil
}
