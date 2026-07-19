package adapter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/notify/domain"
	"github.com/ericfisherdev/nestova/web/components"
)

// emailMemberLister is the narrow read port EmailNotificationSender needs
// to find a household's owner-role members when warning them about a
// bounced/rejected email (NES-141) — mirrors
// tasks/app.HouseholdMemberLister's identical narrowing of
// household.HouseholdRepository (ISP: nothing here needs the rest of
// that much larger interface). household.PostgresRepository satisfies
// this structurally, so the composition root passes it in directly with
// no adapter needed.
type emailMemberLister interface {
	ListMembers(ctx context.Context, householdID household.HouseholdID) ([]*household.Member, error)
}

// EmailNotificationSender is the domain.Sender implementation for the
// email channel (NES-141), wrapping the narrower domain.EmailSender
// (which only knows how to send a subject and two body parts to an
// already-resolved address) with the member-resolution step a full
// Sender needs: looking up the notification's member's current email
// address before ever calling the underlying EmailSender — mirrors
// SMSNotificationSender's identical wrapping of SMSSender.
//
// Unlike SMS, this resolution has no separate enqueue-time readiness
// check to race against: routing.RoutingEnqueuer's own resolveChannel
// accepts an "email" preference unconditionally (email readiness, unlike
// SMS opt-in state, lives in a different bounded context notify does not
// import — see routing.go's own doc), so member resolution happens ONLY
// here, at send time.
//
// Bounce handling (NES-141, sandbox scope): when the wrapped EmailSender
// returns domain.ErrRecipientRejected, this type ALSO — best-effort,
// logged only, never affecting the error Send itself returns —
// downgrades the member's every email preference to in-app (so they stop
// silently missing future notifications routed to an address that does
// not accept mail from this deployment) and enqueues an in-app warning
// to every owner-role member of the household. This is separate from,
// and complementary to, Dispatcher.fallbackToInApp (a channel-agnostic
// mechanism that still re-delivers THIS notification's own content
// in-app regardless of the specific error): one recovers the lost
// message, the other prevents the next one from being lost the same way.
type EmailNotificationSender struct {
	email       domain.EmailSender
	resolver    domain.MemberEmailResolver
	preferences domain.PreferenceRepository
	members     emailMemberLister
	// enqueuer is the RAW outbox (domain.Enqueuer), not whatever
	// RoutingEnqueuer wraps it at the composition root — the owner
	// warning's channel is deliberately fixed to in-app, mirroring
	// Dispatcher.fallbackToInApp's identical reasoning for why
	// re-resolving it through member preference would be wrong here.
	enqueuer domain.Enqueuer
	logger   *slog.Logger
}

// Compile-time assurance the adapter satisfies the port.
var _ domain.Sender = (*EmailNotificationSender)(nil)

// NewEmailNotificationSender constructs an EmailNotificationSender with
// injected dependencies. Panics when any is nil.
func NewEmailNotificationSender(
	email domain.EmailSender,
	resolver domain.MemberEmailResolver,
	preferences domain.PreferenceRepository,
	members emailMemberLister,
	enqueuer domain.Enqueuer,
	logger *slog.Logger,
) *EmailNotificationSender {
	if email == nil {
		panic("adapter: NewEmailNotificationSender requires a non-nil EmailSender")
	}
	if resolver == nil {
		panic("adapter: NewEmailNotificationSender requires a non-nil MemberEmailResolver")
	}
	if preferences == nil {
		panic("adapter: NewEmailNotificationSender requires a non-nil PreferenceRepository")
	}
	if members == nil {
		panic("adapter: NewEmailNotificationSender requires a non-nil member lister")
	}
	if enqueuer == nil {
		panic("adapter: NewEmailNotificationSender requires a non-nil Enqueuer")
	}
	if logger == nil {
		panic("adapter: NewEmailNotificationSender requires a non-nil logger")
	}
	return &EmailNotificationSender{
		email:       email,
		resolver:    resolver,
		preferences: preferences,
		members:     members,
		enqueuer:    enqueuer,
		logger:      logger,
	}
}

// Channel reports the delivery channel this sender handles.
func (s *EmailNotificationSender) Channel() domain.Channel { return domain.ChannelEmail }

// Send resolves n.MemberID's current email address and sends n's title
// and body — rendered as both an HTML and a plain-text part (see
// domain.EmailSender's own doc for why every send carries both) — through
// the wrapped EmailSender.
//
// Every failure branch here is terminal (never retried by this sender
// itself — the underlying EmailSender already exhausted its own retry
// budget for transient AWS failures before ever returning an error to
// this method): a nil MemberID (a household-wide notification was
// somehow routed to email, which should never happen — there is no
// single destination address for a household), a member with no
// resolvable email (any ResolveEmail error — see that port's own doc for
// why its two underlying causes are not distinguished), or the
// EmailSender's own terminal outcome (domain.ErrRecipientRejected or a
// wrapped provider error) all return a non-nil error, which
// Dispatcher.deliver maps to a terminal failure with an in-app fallback
// (NES-139/141) — see that method's own doc. A rejection additionally
// triggers this type's own bounce handling — see the type's own doc.
func (s *EmailNotificationSender) Send(ctx context.Context, n *domain.Notification) error {
	if n == nil {
		return errors.New("adapter: email send: nil notification")
	}
	if n.MemberID == nil {
		return errors.New("adapter: email send: notification has no member to address")
	}

	address, err := s.resolver.ResolveEmail(ctx, *n.MemberID)
	if err != nil {
		return fmt.Errorf("adapter: email send: resolve email: %w", err)
	}

	view := components.EmailNotificationView{Title: n.Title, Body: n.Body}
	htmlBody, err := renderEmailHTML(ctx, view)
	if err != nil {
		return fmt.Errorf("adapter: email send: render html: %w", err)
	}
	textBody := components.EmailNotificationPlainText(view)

	if _, err := s.email.Send(ctx, address, n.Title, htmlBody, textBody); err != nil {
		if errors.Is(err, domain.ErrRecipientRejected) {
			s.handleRejection(ctx, n)
		}
		return err
	}
	return nil
}

// renderEmailHTML renders view through components.EmailNotificationHTML
// to a string, the shape domain.EmailSender.Send expects.
func renderEmailHTML(ctx context.Context, view components.EmailNotificationView) (string, error) {
	var sb strings.Builder
	if err := components.EmailNotificationHTML(view).Render(ctx, &sb); err != nil {
		return "", fmt.Errorf("adapter: render email html: %w", err)
	}
	return sb.String(), nil
}

// handleRejection performs NES-141's bounce-handling side effects after
// a terminal recipient rejection — see this type's own doc. Both steps
// are best-effort: a failure here is logged and does not affect Send's
// own return value, since Send's caller already has the original
// terminal error to record regardless of whether these side effects
// succeed.
func (s *EmailNotificationSender) handleRejection(ctx context.Context, n *domain.Notification) {
	memberID := *n.MemberID
	if err := s.preferences.DowngradeChannel(ctx, memberID, domain.ChannelEmail, domain.ChannelInApp); err != nil {
		s.logger.Error("email send: downgrade preference after rejection failed",
			"member_id", memberID.String(),
			"error", err,
		)
	}
	s.warnOwners(ctx, n)
}

// warnOwners enqueues an in-app notification to every owner-role member
// of n's household, naming the affected member by their display name —
// resolved from the SAME ListMembers call used to find the owners, not a
// second lookup, mirroring RewardService.notifyParentsOfRedemption's
// identical single-lookup pattern.
func (s *EmailNotificationSender) warnOwners(ctx context.Context, n *domain.Notification) {
	members, err := s.members.ListMembers(ctx, n.HouseholdID)
	if err != nil {
		s.logger.Error("email send: list members for bounce warning failed",
			"household_id", n.HouseholdID.String(),
			"error", err,
		)
		return
	}

	affectedName := "A member"
	for _, m := range members {
		if m.ID == *n.MemberID {
			affectedName = m.DisplayName
			break
		}
	}

	for _, m := range members {
		if m.Role != household.RoleOwner {
			continue
		}
		ownerID := m.ID
		warning := &domain.Notification{
			ID:          domain.NewNotificationID(),
			HouseholdID: n.HouseholdID,
			MemberID:    &ownerID,
			Channel:     domain.ChannelInApp,
			Title:       "Email delivery stopped for a member",
			Body: fmt.Sprintf("%s's email could not be delivered, so their notifications have switched to in-app only. Ask them to check their address in settings.",
				affectedName),
			ScheduledFor: time.Now(),
			Status:       domain.StatusPending,
		}
		if err := s.enqueuer.Enqueue(ctx, warning); err != nil {
			s.logger.Error("email send: bounce warning enqueue failed",
				"member_id", ownerID.String(),
				"error", err,
			)
		}
	}
}
