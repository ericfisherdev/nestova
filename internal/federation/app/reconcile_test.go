package app_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ericfisherdev/nestova/internal/federation/app"
	"github.com/ericfisherdev/nestova/internal/federation/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

func strPtr(s string) *string { return &s }

// fakeBindingCredential is a scripted bindingCredential: it returns baseURL/
// apiKey/err exactly as configured, and records how many times it was
// called.
type fakeBindingCredential struct {
	baseURL string
	apiKey  string
	err     error
	calls   int
}

func (f *fakeBindingCredential) Credential(_ context.Context, _ household.HouseholdID) (string, string, error) {
	f.calls++
	return f.baseURL, f.apiKey, f.err
}

// fakeAccountReader is a scripted accountReader.
type fakeAccountReader struct {
	accounts []domain.RemoteAccount
	err      error
	calls    int
}

func (f *fakeAccountReader) ReadAccounts(_ context.Context, _, _ string, _ household.HouseholdID) ([]domain.RemoteAccount, error) {
	f.calls++
	return f.accounts, f.err
}

// fakeProvisioner is a scripted provisioner: it can fail unconditionally
// (err) or fail for one specific member id (failFor), and otherwise
// returns a synthesized account whose RemoteUserID mirrors what was
// requested (or a fresh "created-<memberID>" id for a Create).
type fakeProvisioner struct {
	err      error
	failFor  map[household.MemberID]error
	calls    []domain.ProvisionRequest
	memberID []household.MemberID
}

func (f *fakeProvisioner) Provision(_ context.Context, _, _ string, _ household.HouseholdID, memberID household.MemberID, req domain.ProvisionRequest) (domain.RemoteAccount, error) {
	f.calls = append(f.calls, req)
	f.memberID = append(f.memberID, memberID)
	if f.err != nil {
		return domain.RemoteAccount{}, f.err
	}
	if f.failFor != nil {
		if err, ok := f.failFor[memberID]; ok {
			return domain.RemoteAccount{}, err
		}
	}
	if req.Link != nil {
		return domain.RemoteAccount{RemoteUserID: req.Link.RemoteUserID}, nil
	}
	return domain.RemoteAccount{RemoteUserID: "created-" + memberID.String(), DisplayName: req.Create.DisplayName, Email: req.Create.Email}, nil
}

// fakeMemberReader is a scripted memberReader.
type fakeMemberReader struct {
	members []domain.MemberCandidate
	err     error
}

func (f *fakeMemberReader) MembersByHousehold(_ context.Context, _ household.HouseholdID) ([]domain.MemberCandidate, error) {
	return f.members, f.err
}

// fakeMemberLinkRepository is an in-memory domain.MemberLinkRepository
// keyed by household id, mirroring fakeLinkRepo's own (link_test.go)
// in-memory-map style so ReconciliationService can be exercised without a
// database.
type fakeMemberLinkRepository struct {
	byHousehold map[household.HouseholdID][]domain.MemberLink
	byRemote    map[string]household.MemberID
	putCalls    int
}

func newFakeMemberLinkRepository() *fakeMemberLinkRepository {
	return &fakeMemberLinkRepository{byHousehold: map[household.HouseholdID][]domain.MemberLink{}, byRemote: map[string]household.MemberID{}}
}

func (f *fakeMemberLinkRepository) ListByHousehold(_ context.Context, householdID household.HouseholdID) ([]domain.MemberLink, error) {
	return f.byHousehold[householdID], nil
}

func (f *fakeMemberLinkRepository) Put(_ context.Context, link domain.MemberLink) error {
	f.putCalls++
	if existing, ok := f.byRemote[link.RemoteUserID]; ok && existing != link.MemberID {
		return domain.ErrLinkConflict
	}
	f.byRemote[link.RemoteUserID] = link.MemberID
	f.byHousehold[link.HouseholdID] = append(f.byHousehold[link.HouseholdID], link)
	return nil
}

func mustReconciliationService(t *testing.T, binding *fakeBindingCredential, accounts *fakeAccountReader, provision *fakeProvisioner, links domain.MemberLinkRepository, members *fakeMemberReader) *app.ReconciliationService {
	t.Helper()
	svc, err := app.NewReconciliationService(binding, accounts, provision, links, members, discardLogger())
	if err != nil {
		t.Fatalf("NewReconciliationService: %v", err)
	}
	return svc
}

func TestNewReconciliationServiceRequiresDependencies(t *testing.T) {
	binding := &fakeBindingCredential{}
	accounts := &fakeAccountReader{}
	provision := &fakeProvisioner{}
	links := newFakeMemberLinkRepository()
	members := &fakeMemberReader{}
	logger := discardLogger()

	if _, err := app.NewReconciliationService(nil, accounts, provision, links, members, logger); err == nil {
		t.Error("NewReconciliationService(nil binding) error = nil, want error")
	}
	if _, err := app.NewReconciliationService(binding, nil, provision, links, members, logger); err == nil {
		t.Error("NewReconciliationService(nil accounts) error = nil, want error")
	}
	if _, err := app.NewReconciliationService(binding, accounts, nil, links, members, logger); err == nil {
		t.Error("NewReconciliationService(nil provision) error = nil, want error")
	}
	if _, err := app.NewReconciliationService(binding, accounts, provision, nil, members, logger); err == nil {
		t.Error("NewReconciliationService(nil links) error = nil, want error")
	}
	if _, err := app.NewReconciliationService(binding, accounts, provision, links, nil, logger); err == nil {
		t.Error("NewReconciliationService(nil members) error = nil, want error")
	}
	if _, err := app.NewReconciliationService(binding, accounts, provision, links, members, nil); err == nil {
		t.Error("NewReconciliationService(nil logger) error = nil, want error")
	}
}

func TestReconciliationServiceProposalsNotAttached(t *testing.T) {
	binding := &fakeBindingCredential{err: domain.ErrLinkNotFound}
	svc := mustReconciliationService(t, binding, &fakeAccountReader{}, &fakeProvisioner{}, newFakeMemberLinkRepository(), &fakeMemberReader{})

	_, err := svc.Proposals(context.Background(), household.NewHouseholdID())
	if !errors.Is(err, app.ErrNotAttached) {
		t.Fatalf("Proposals() error = %v, want ErrNotAttached", err)
	}
}

func TestReconciliationServiceProposalsReturnsOneRowPerMember(t *testing.T) {
	matched := household.NewMemberID()
	unmatched := household.NewMemberID()
	linked := household.NewMemberID()
	hh := household.NewHouseholdID()

	binding := &fakeBindingCredential{baseURL: "https://nestorage.example.ts.net", apiKey: "key"}
	members := &fakeMemberReader{members: []domain.MemberCandidate{
		{MemberID: matched, DisplayName: "Maya", Email: strPtr("maya@example.com")},
		{MemberID: unmatched, DisplayName: "Sam", Email: strPtr("sam@example.com")},
		{MemberID: linked, DisplayName: "Already Linked", Email: strPtr("linked@example.com")},
	}}
	accounts := &fakeAccountReader{accounts: []domain.RemoteAccount{
		{RemoteUserID: "remote-maya", Email: "maya@example.com", DisplayName: "Maya R."},
		{RemoteUserID: "remote-linked", Email: "linked@example.com", DisplayName: "Linked R."},
	}}
	links := newFakeMemberLinkRepository()
	links.byHousehold[hh] = []domain.MemberLink{{MemberID: linked, HouseholdID: hh, RemoteUserID: "remote-linked", LinkedVia: domain.LinkOriginEmail}}

	svc := mustReconciliationService(t, binding, accounts, &fakeProvisioner{}, links, members)
	rows, err := svc.Proposals(context.Background(), hh)
	if err != nil {
		t.Fatalf("Proposals: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("Proposals() returned %d rows, want 3", len(rows))
	}

	byMember := make(map[household.MemberID]app.Proposal, len(rows))
	for _, r := range rows {
		byMember[r.MemberID] = r
	}

	matchedRow := byMember[matched]
	if matchedRow.Linked {
		t.Error("matched member row.Linked = true, want false")
	}
	if matchedRow.Proposed == nil || matchedRow.Proposed.RemoteUserID != "remote-maya" {
		t.Errorf("matched member row.Proposed = %v, want remote-maya", matchedRow.Proposed)
	}
	if matchedRow.Reason != domain.LinkOriginEmail {
		t.Errorf("matched member row.Reason = %v, want LinkOriginEmail", matchedRow.Reason)
	}

	unmatchedRow := byMember[unmatched]
	if unmatchedRow.Linked || unmatchedRow.Proposed != nil {
		t.Errorf("unmatched member row = %+v, want no-match", unmatchedRow)
	}

	linkedRow := byMember[linked]
	if !linkedRow.Linked || linkedRow.LinkedRemoteUserID != "remote-linked" {
		t.Errorf("linked member row = %+v, want Linked=true, LinkedRemoteUserID=remote-linked", linkedRow)
	}
	if linkedRow.Proposed != nil {
		t.Errorf("linked member row.Proposed = %v, want nil", linkedRow.Proposed)
	}
}

func TestReconciliationServiceProposalsWritesNothing(t *testing.T) {
	hh := household.NewHouseholdID()
	member := household.NewMemberID()
	binding := &fakeBindingCredential{baseURL: "https://nestorage.example.ts.net", apiKey: "key"}
	members := &fakeMemberReader{members: []domain.MemberCandidate{{MemberID: member, Email: strPtr("maya@example.com")}}}
	accounts := &fakeAccountReader{accounts: []domain.RemoteAccount{{RemoteUserID: "remote-1", Email: "maya@example.com"}}}
	provision := &fakeProvisioner{}
	links := newFakeMemberLinkRepository()

	svc := mustReconciliationService(t, binding, accounts, provision, links, members)
	if _, err := svc.Proposals(context.Background(), hh); err != nil {
		t.Fatalf("Proposals: %v", err)
	}
	if len(provision.calls) != 0 {
		t.Errorf("provision.calls = %d, want 0 — Proposals must never provision", len(provision.calls))
	}
	if links.putCalls != 0 {
		t.Errorf("links.putCalls = %d, want 0 — Proposals must never write a link", links.putCalls)
	}
}

func TestReconciliationServiceConfirmNotAttached(t *testing.T) {
	binding := &fakeBindingCredential{err: domain.ErrLinkNotFound}
	svc := mustReconciliationService(t, binding, &fakeAccountReader{}, &fakeProvisioner{}, newFakeMemberLinkRepository(), &fakeMemberReader{})

	_, err := svc.Confirm(context.Background(), household.NewHouseholdID(), []app.Decision{{MemberID: household.NewMemberID()}})
	if !errors.Is(err, app.ErrNotAttached) {
		t.Fatalf("Confirm() error = %v, want ErrNotAttached", err)
	}
}

func TestReconciliationServiceConfirmRejectsMemberOutsideHousehold(t *testing.T) {
	hh := household.NewHouseholdID()
	inHousehold := household.NewMemberID()
	outsider := household.NewMemberID()
	binding := &fakeBindingCredential{baseURL: "https://nestorage.example.ts.net", apiKey: "key"}
	members := &fakeMemberReader{members: []domain.MemberCandidate{{MemberID: inHousehold, DisplayName: "Maya"}}}
	provision := &fakeProvisioner{}

	svc := mustReconciliationService(t, binding, &fakeAccountReader{}, provision, newFakeMemberLinkRepository(), members)
	_, err := svc.Confirm(context.Background(), hh, []app.Decision{{MemberID: outsider, RemoteUserID: strPtr("remote-1")}})
	if !errors.Is(err, app.ErrMemberOutsideHousehold) {
		t.Fatalf("Confirm() error = %v, want ErrMemberOutsideHousehold", err)
	}
	if len(provision.calls) != 0 {
		t.Errorf("provision.calls = %d, want 0 — an out-of-household decision must never reach Provision", len(provision.calls))
	}
}

func TestReconciliationServiceConfirmDecisionFreeSubmitWritesNothing(t *testing.T) {
	binding := &fakeBindingCredential{baseURL: "https://nestorage.example.ts.net", apiKey: "key"}
	provision := &fakeProvisioner{}
	links := newFakeMemberLinkRepository()

	svc := mustReconciliationService(t, binding, &fakeAccountReader{}, provision, links, &fakeMemberReader{})
	outcome, err := svc.Confirm(context.Background(), household.NewHouseholdID(), nil)
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if len(outcome.Succeeded) != 0 || len(outcome.Failed) != 0 {
		t.Errorf("Confirm(no decisions) = %+v, want an empty Outcome", outcome)
	}
	if len(provision.calls) != 0 || links.putCalls != 0 {
		t.Error("Confirm(no decisions) must call neither Provision nor Put")
	}
}

func TestReconciliationServiceConfirmLinksAcceptedMatch(t *testing.T) {
	hh := household.NewHouseholdID()
	member := household.NewMemberID()
	binding := &fakeBindingCredential{baseURL: "https://nestorage.example.ts.net", apiKey: "key"}
	members := &fakeMemberReader{members: []domain.MemberCandidate{{MemberID: member, DisplayName: "Maya", Email: strPtr("maya@example.com")}}}
	provision := &fakeProvisioner{}
	links := newFakeMemberLinkRepository()

	svc := mustReconciliationService(t, binding, &fakeAccountReader{}, provision, links, members)
	outcome, err := svc.Confirm(context.Background(), hh, []app.Decision{{MemberID: member, RemoteUserID: strPtr("remote-1")}})
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if len(outcome.Succeeded) != 1 || outcome.Succeeded[0] != member {
		t.Fatalf("Confirm() outcome.Succeeded = %v, want [%v]", outcome.Succeeded, member)
	}
	if len(provision.calls) != 1 || provision.calls[0].Link == nil || provision.calls[0].Link.RemoteUserID != "remote-1" {
		t.Fatalf("Provision call = %+v, want a Link request for remote-1", provision.calls)
	}
	stored := links.byHousehold[hh]
	if len(stored) != 1 || stored[0].RemoteUserID != "remote-1" || stored[0].LinkedVia != domain.LinkOriginEmail {
		t.Fatalf("stored link = %+v, want remote-1 via email", stored)
	}
}

func TestReconciliationServiceConfirmManualLinkRecordsManualOrigin(t *testing.T) {
	hh := household.NewHouseholdID()
	member := household.NewMemberID()
	binding := &fakeBindingCredential{baseURL: "https://nestorage.example.ts.net", apiKey: "key"}
	members := &fakeMemberReader{members: []domain.MemberCandidate{{MemberID: member, DisplayName: "Maya"}}}
	links := newFakeMemberLinkRepository()

	svc := mustReconciliationService(t, binding, &fakeAccountReader{}, &fakeProvisioner{}, links, members)
	if _, err := svc.Confirm(context.Background(), hh, []app.Decision{{MemberID: member, RemoteUserID: strPtr("remote-1"), Manual: true}}); err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	stored := links.byHousehold[hh]
	if len(stored) != 1 || stored[0].LinkedVia != domain.LinkOriginManual {
		t.Fatalf("stored link = %+v, want LinkOriginManual", stored)
	}
}

func TestReconciliationServiceConfirmCreatesNewAccountForNoMatch(t *testing.T) {
	hh := household.NewHouseholdID()
	member := household.NewMemberID()
	binding := &fakeBindingCredential{baseURL: "https://nestorage.example.ts.net", apiKey: "key"}
	members := &fakeMemberReader{members: []domain.MemberCandidate{
		{MemberID: member, DisplayName: "Sam", Role: household.RoleAdult, Email: strPtr("sam@example.com")},
	}}
	provision := &fakeProvisioner{}
	links := newFakeMemberLinkRepository()

	svc := mustReconciliationService(t, binding, &fakeAccountReader{}, provision, links, members)
	outcome, err := svc.Confirm(context.Background(), hh, []app.Decision{{MemberID: member}})
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if len(outcome.Succeeded) != 1 {
		t.Fatalf("Confirm() outcome = %+v, want one success", outcome)
	}
	if len(provision.calls) != 1 || provision.calls[0].Create == nil {
		t.Fatalf("Provision call = %+v, want a Create request", provision.calls)
	}
	create := provision.calls[0].Create
	if create.DisplayName != "Sam" || create.Email != "sam@example.com" || create.Role != "admin" {
		t.Errorf("Create request = %+v, want Sam/sam@example.com/admin", create)
	}
	stored := links.byHousehold[hh]
	if len(stored) != 1 || stored[0].LinkedVia != domain.LinkOriginCreated {
		t.Fatalf("stored link = %+v, want LinkOriginCreated", stored)
	}
}

func TestReconciliationServiceConfirmSkipsAlreadyLinkedMembers(t *testing.T) {
	hh := household.NewHouseholdID()
	member := household.NewMemberID()
	binding := &fakeBindingCredential{baseURL: "https://nestorage.example.ts.net", apiKey: "key"}
	members := &fakeMemberReader{members: []domain.MemberCandidate{{MemberID: member, DisplayName: "Maya"}}}
	provision := &fakeProvisioner{}
	links := newFakeMemberLinkRepository()
	links.byHousehold[hh] = []domain.MemberLink{{MemberID: member, HouseholdID: hh, RemoteUserID: "remote-1", LinkedVia: domain.LinkOriginEmail}}

	svc := mustReconciliationService(t, binding, &fakeAccountReader{}, provision, links, members)
	outcome, err := svc.Confirm(context.Background(), hh, []app.Decision{{MemberID: member, RemoteUserID: strPtr("remote-2")}})
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if len(outcome.Succeeded) != 0 || len(outcome.Failed) != 0 {
		t.Errorf("Confirm() outcome = %+v, want empty — an already-linked member is silently skipped", outcome)
	}
	if len(provision.calls) != 0 {
		t.Errorf("provision.calls = %d, want 0 — an already-linked member must never reach Provision", len(provision.calls))
	}
}

func TestReconciliationServiceConfirmPartialFailureKeepsSuccessesAndRetryProvisionsOnlyFailures(t *testing.T) {
	hh := household.NewHouseholdID()
	ok := household.NewMemberID()
	bad := household.NewMemberID()
	binding := &fakeBindingCredential{baseURL: "https://nestorage.example.ts.net", apiKey: "key"}
	members := &fakeMemberReader{members: []domain.MemberCandidate{
		{MemberID: ok, DisplayName: "Ok"},
		{MemberID: bad, DisplayName: "Bad"},
	}}
	failure := errors.New("boom")
	provision := &fakeProvisioner{failFor: map[household.MemberID]error{bad: failure}}
	links := newFakeMemberLinkRepository()

	svc := mustReconciliationService(t, binding, &fakeAccountReader{}, provision, links, members)
	decisions := []app.Decision{
		{MemberID: ok, RemoteUserID: strPtr("remote-ok")},
		{MemberID: bad, RemoteUserID: strPtr("remote-bad")},
	}
	outcome, err := svc.Confirm(context.Background(), hh, decisions)
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if len(outcome.Succeeded) != 1 || outcome.Succeeded[0] != ok {
		t.Fatalf("outcome.Succeeded = %v, want [%v]", outcome.Succeeded, ok)
	}
	if len(outcome.Failed) != 1 {
		t.Fatalf("outcome.Failed = %v, want exactly one failure", outcome.Failed)
	}
	if !errors.Is(outcome.Failed[bad], failure) {
		t.Errorf("outcome.Failed[bad] = %v, want it to wrap %v", outcome.Failed[bad], failure)
	}
	if len(links.byHousehold[hh]) != 1 {
		t.Fatalf("stored links = %v, want exactly the successful one", links.byHousehold[hh])
	}

	// Retry with the SAME decisions: ok is now already-linked and is
	// skipped, so only bad reaches Provision a second time.
	provision.calls = nil
	provision.memberID = nil
	provision.failFor = nil // simulate the transient failure clearing
	outcome, err = svc.Confirm(context.Background(), hh, decisions)
	if err != nil {
		t.Fatalf("retry Confirm: %v", err)
	}
	if len(provision.calls) != 1 || provision.memberID[0] != bad {
		t.Fatalf("retry provisioned %v, want exactly [%v]", provision.memberID, bad)
	}
	if len(outcome.Succeeded) != 1 || outcome.Succeeded[0] != bad {
		t.Fatalf("retry outcome.Succeeded = %v, want [%v]", outcome.Succeeded, bad)
	}
}

func TestReconciliationServiceConfirmLinkConflictPassthrough(t *testing.T) {
	hh := household.NewHouseholdID()
	member := household.NewMemberID()
	binding := &fakeBindingCredential{baseURL: "https://nestorage.example.ts.net", apiKey: "key"}
	members := &fakeMemberReader{members: []domain.MemberCandidate{{MemberID: member, DisplayName: "Maya"}}}
	provision := &fakeProvisioner{err: domain.ErrLinkConflict}
	links := newFakeMemberLinkRepository()

	svc := mustReconciliationService(t, binding, &fakeAccountReader{}, provision, links, members)
	outcome, err := svc.Confirm(context.Background(), hh, []app.Decision{{MemberID: member, RemoteUserID: strPtr("remote-1")}})
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if !errors.Is(outcome.Failed[member], domain.ErrLinkConflict) {
		t.Errorf("outcome.Failed[member] = %v, want it to wrap ErrLinkConflict", outcome.Failed[member])
	}
}
