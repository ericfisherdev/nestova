package domain_test

import (
	"testing"

	"github.com/ericfisherdev/nestova/internal/federation/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

func strPtr(s string) *string { return &s }

func TestProposeMatchesExactEmailProposes(t *testing.T) {
	member := household.NewMemberID()
	members := []domain.MemberCandidate{
		{MemberID: member, DisplayName: "Maya", Email: strPtr("maya@example.com")},
	}
	accounts := []domain.RemoteAccount{
		{RemoteUserID: "remote-1", Email: "maya@example.com", DisplayName: "Maya"},
	}

	got := domain.ProposeMatches(members, accounts, nil)
	if len(got) != 1 {
		t.Fatalf("ProposeMatches() returned %d proposals, want 1", len(got))
	}
	if got[0].MemberID != member {
		t.Errorf("MemberID = %v, want %v", got[0].MemberID, member)
	}
	if got[0].RemoteUserID == nil || *got[0].RemoteUserID != "remote-1" {
		t.Errorf("RemoteUserID = %v, want %q", got[0].RemoteUserID, "remote-1")
	}
	if got[0].MatchReason != domain.LinkOriginEmail {
		t.Errorf("MatchReason = %v, want %v", got[0].MatchReason, domain.LinkOriginEmail)
	}
}

func TestProposeMatchesDifferingCaseMatches(t *testing.T) {
	member := household.NewMemberID()
	members := []domain.MemberCandidate{
		{MemberID: member, Email: strPtr("Maya@Example.com")},
	}
	accounts := []domain.RemoteAccount{
		{RemoteUserID: "remote-1", Email: "maya@example.com"},
	}

	got := domain.ProposeMatches(members, accounts, nil)
	if len(got) != 1 || got[0].RemoteUserID == nil || *got[0].RemoteUserID != "remote-1" {
		t.Fatalf("ProposeMatches() = %+v, want a case-insensitive match on remote-1", got)
	}
}

func TestProposeMatchesDuplicateRemoteEmailsYieldUnmatched(t *testing.T) {
	memberA := household.NewMemberID()
	memberB := household.NewMemberID()
	members := []domain.MemberCandidate{
		{MemberID: memberA, Email: strPtr("shared@example.com")},
		{MemberID: memberB, Email: strPtr("other@example.com")},
	}
	accounts := []domain.RemoteAccount{
		{RemoteUserID: "remote-1", Email: "shared@example.com"},
		{RemoteUserID: "remote-2", Email: "shared@example.com"},
		{RemoteUserID: "remote-3", Email: "other@example.com"},
	}

	got := domain.ProposeMatches(members, accounts, nil)
	byMember := make(map[household.MemberID]domain.MatchProposal, len(got))
	for _, p := range got {
		byMember[p.MemberID] = p
	}

	if p := byMember[memberA]; p.RemoteUserID != nil {
		t.Errorf("memberA (ambiguous duplicate remote email) proposal = %+v, want unmatched", p)
	}
	if p := byMember[memberB]; p.RemoteUserID == nil || *p.RemoteUserID != "remote-3" {
		t.Errorf("memberB (unique email) proposal = %+v, want remote-3", p)
	}
}

func TestProposeMatchesNullEmailYieldsUnmatched(t *testing.T) {
	member := household.NewMemberID()
	members := []domain.MemberCandidate{
		{MemberID: member, Email: nil},
	}
	accounts := []domain.RemoteAccount{
		{RemoteUserID: "remote-1", Email: "someone@example.com"},
	}

	got := domain.ProposeMatches(members, accounts, nil)
	if len(got) != 1 {
		t.Fatalf("ProposeMatches() returned %d proposals, want 1", len(got))
	}
	if got[0].RemoteUserID != nil {
		t.Errorf("RemoteUserID = %v, want nil for a member with no email on file", got[0].RemoteUserID)
	}
}

func TestProposeMatchesExcludesAlreadyLinkedMembersAndAccounts(t *testing.T) {
	linkedMember := household.NewMemberID()
	unlinkedMember := household.NewMemberID()
	members := []domain.MemberCandidate{
		{MemberID: linkedMember, Email: strPtr("linked@example.com")},
		{MemberID: unlinkedMember, Email: strPtr("fresh@example.com")},
	}
	accounts := []domain.RemoteAccount{
		{RemoteUserID: "remote-linked", Email: "linked@example.com"},
		{RemoteUserID: "remote-fresh", Email: "fresh@example.com"},
	}
	existingLinks := []domain.MemberLink{
		{MemberID: linkedMember, RemoteUserID: "remote-linked", LinkedVia: domain.LinkOriginEmail},
	}

	got := domain.ProposeMatches(members, accounts, existingLinks)
	if len(got) != 1 {
		t.Fatalf("ProposeMatches() returned %d proposals, want 1 (the linked member excluded)", len(got))
	}
	if got[0].MemberID != unlinkedMember {
		t.Errorf("proposal MemberID = %v, want %v", got[0].MemberID, unlinkedMember)
	}
	if got[0].RemoteUserID == nil || *got[0].RemoteUserID != "remote-fresh" {
		t.Errorf("proposal RemoteUserID = %v, want remote-fresh", got[0].RemoteUserID)
	}
}

func TestProposeMatchesOutputOrderIsDeterministic(t *testing.T) {
	first := household.NewMemberID()
	second := household.NewMemberID()
	third := household.NewMemberID()
	members := []domain.MemberCandidate{
		{MemberID: first, Email: strPtr("first@example.com")},
		{MemberID: second, Email: strPtr("second@example.com")},
		{MemberID: third, Email: nil},
	}

	got := domain.ProposeMatches(members, nil, nil)
	if len(got) != 3 {
		t.Fatalf("ProposeMatches() returned %d proposals, want 3", len(got))
	}
	if got[0].MemberID != first || got[1].MemberID != second || got[2].MemberID != third {
		t.Fatalf("ProposeMatches() order = %v, %v, %v; want input order", got[0].MemberID, got[1].MemberID, got[2].MemberID)
	}
}

func TestLinkOriginValidAndParse(t *testing.T) {
	for _, valid := range []domain.LinkOrigin{domain.LinkOriginEmail, domain.LinkOriginManual, domain.LinkOriginCreated} {
		if !valid.Valid() {
			t.Errorf("%q.Valid() = false, want true", valid)
		}
		parsed, err := domain.ParseLinkOrigin(valid.String())
		if err != nil {
			t.Errorf("ParseLinkOrigin(%q): %v", valid, err)
		}
		if parsed != valid {
			t.Errorf("ParseLinkOrigin(%q) = %q, want %q", valid, parsed, valid)
		}
	}

	if (domain.LinkOrigin("bogus")).Valid() {
		t.Error(`LinkOrigin("bogus").Valid() = true, want false`)
	}
	if _, err := domain.ParseLinkOrigin("bogus"); err == nil {
		t.Error(`ParseLinkOrigin("bogus") error = nil, want error`)
	}
}
