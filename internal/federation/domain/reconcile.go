package domain

import (
	"strings"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// RemoteAccount is one Nestorage account as reconciliation sees it — read
// via a NestorageClient's ReadAccounts, NSTR-101's account-read response
// projected onto exactly the fields matching and display need, and nothing
// else (there is no password hash or any other sensitive field to leak).
type RemoteAccount struct {
	RemoteUserID string
	Email        string
	DisplayName  string
}

// MemberCandidate is one household member as reconciliation sees it: enough
// to match on (Email), enough to render for a human (DisplayName), and
// enough to provision a brand new Nestorage account from (Role) — never
// anything more (ISP: the exact slice of household.Member and its login
// email the federation context needs, mirroring notify's own
// MemberContact precedent of a context-scoped read model rather than
// depending on household.Member directly).
type MemberCandidate struct {
	MemberID    household.MemberID
	DisplayName string
	Role        household.Role
	// Email is nil when the member has no login email on file
	// (member.email, citext, nullable) — ProposeMatches always treats a
	// nil email as unmatched, never a wildcard.
	Email *string
}

// MatchProposal is one household member's reconciliation candidacy, as
// ProposeMatches computes it: either a proposed already-existing Nestorage
// account together with the reason it was proposed, or an explicit
// no-match (RemoteUserID nil, MatchReason the zero value).
type MatchProposal struct {
	MemberID     household.MemberID
	RemoteUserID *string
	MatchReason  LinkOrigin
}

// ProposeMatches computes, for every member in members NOT already present
// in existingLinks, whether exactly one remote account in accounts (also
// excluding any already present in existingLinks) shares its login email.
//
// The heuristic is deliberately narrow: case-insensitive exact email
// equality is the ONLY signal that ever proposes a match. Display name is
// rendered for a human to review but is never compared. A member with no
// email on file is always unmatched. The pairing must additionally be
// UNIQUE in both directions — if more than one candidate remote account
// shares a member's email, or more than one candidate member shares a
// remote account's email, every row on the ambiguous side is left
// unmatched rather than guessing, because a wrong automatic link silently
// hands one person's storage history to another and a human clicking
// through a plausible-looking list is a likelier failure than a human
// noticing an omission.
//
// The returned slice has one entry per non-linked member, in the same
// order as members — deterministic, since it never depends on map
// iteration order.
func ProposeMatches(members []MemberCandidate, accounts []RemoteAccount, existingLinks []MemberLink) []MatchProposal {
	linkedMemberIDs, linkedRemoteIDs := linkedSets(existingLinks)

	candidateAccounts := make([]RemoteAccount, 0, len(accounts))
	for _, a := range accounts {
		if !linkedRemoteIDs[a.RemoteUserID] {
			candidateAccounts = append(candidateAccounts, a)
		}
	}
	accountEmailCounts := make(map[string]int, len(candidateAccounts))
	for _, a := range candidateAccounts {
		accountEmailCounts[strings.ToLower(a.Email)]++
	}

	memberEmailCounts := make(map[string]int, len(members))
	for _, m := range members {
		if linkedMemberIDs[m.MemberID] || m.Email == nil {
			continue
		}
		memberEmailCounts[strings.ToLower(*m.Email)]++
	}

	proposals := make([]MatchProposal, 0, len(members))
	for _, m := range members {
		if linkedMemberIDs[m.MemberID] {
			continue
		}
		proposals = append(proposals, proposeForMember(m, candidateAccounts, accountEmailCounts, memberEmailCounts))
	}
	return proposals
}

// linkedSets indexes existingLinks by member id and remote user id, so
// ProposeMatches can exclude both an already-linked member and an
// already-linked remote account from candidacy in one pass each.
func linkedSets(existingLinks []MemberLink) (memberIDs map[household.MemberID]bool, remoteIDs map[string]bool) {
	memberIDs = make(map[household.MemberID]bool, len(existingLinks))
	remoteIDs = make(map[string]bool, len(existingLinks))
	for _, l := range existingLinks {
		memberIDs[l.MemberID] = true
		remoteIDs[l.RemoteUserID] = true
	}
	return memberIDs, remoteIDs
}

// proposeForMember returns m's own MatchProposal: a unique email match when
// one exists, or an explicit no-match otherwise.
func proposeForMember(m MemberCandidate, candidateAccounts []RemoteAccount, accountEmailCounts, memberEmailCounts map[string]int) MatchProposal {
	proposal := MatchProposal{MemberID: m.MemberID}
	if m.Email == nil {
		return proposal
	}
	key := strings.ToLower(*m.Email)
	if accountEmailCounts[key] != 1 || memberEmailCounts[key] != 1 {
		return proposal
	}
	for _, a := range candidateAccounts {
		if strings.EqualFold(a.Email, *m.Email) {
			remoteUserID := a.RemoteUserID
			proposal.RemoteUserID = &remoteUserID
			proposal.MatchReason = LinkOriginEmail
			return proposal
		}
	}
	return proposal
}

// ProvisionRequest is NestorageClient.Provision's own request: exactly one
// of Link or Create, mirroring NSTR-101's PUT
// /api/v1/federation/members/{member_id} contract ("exactly one of link or
// account must be set").
type ProvisionRequest struct {
	Link   *ProvisionLink
	Create *ProvisionCreate
}

// ProvisionLink pairs a member with an already-existing Nestorage account —
// the accepted proposal, or a human's own manual pairing.
type ProvisionLink struct {
	RemoteUserID string
}

// ProvisionCreate carries the profile NSTR-101's account-create/update
// variant needs to provision a brand new Nestorage account for a member
// with no accepted match.
type ProvisionCreate struct {
	DisplayName string
	Email       string
	// Role is the already-mapped Nestorage role ("admin" or "member") —
	// nestova's own three-way household.Role collapses to Nestorage's
	// two-way role at the app layer, never here.
	Role string
}
