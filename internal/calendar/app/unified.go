package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	calendardomain "github.com/ericfisherdev/nestova/internal/calendar/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	subscriptionsapp "github.com/ericfisherdev/nestova/internal/subscriptions/app"
	subscriptionsdomain "github.com/ericfisherdev/nestova/internal/subscriptions/domain"
	tasksdomain "github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// CalendarItemKind classifies a unified calendar item by its source.
type CalendarItemKind string

const (
	// KindEvent is a cached external calendar event.
	KindEvent CalendarItemKind = "event"
	// KindTask is a task instance due date.
	KindTask CalendarItemKind = "task"
	// KindRenewal is a subscription renewal occurrence.
	KindRenewal CalendarItemKind = "renewal"
)

// maxRenewalOccurrences bounds how many renewal occurrences a single subscription
// can contribute to one range, guarding against a degenerate cycle producing an
// unbounded projection.
const maxRenewalOccurrences = 366

// CalendarItem is one entry in the unified household calendar: an external event,
// a task due date, or a subscription renewal. Start is the item's start (events)
// or date (tasks/renewals); End is the event end (zero for tasks/renewals).
// MemberColor is the A-Hearth color of the attributed member (task assignee or
// subscription payer), empty for unattributed items.
type CalendarItem struct {
	Kind        CalendarItemKind
	Title       string
	Start       time.Time
	End         time.Time
	AllDay      bool
	SourceID    string
	MemberColor string
}

// Narrow read ports the unified view depends on (ISP); the pgx repositories and
// the household repository satisfy them.
type (
	externalEventLister interface {
		ListByHouseholdRange(ctx context.Context, householdID household.HouseholdID, from, to time.Time) ([]*calendardomain.ExternalEvent, error)
	}
	taskInstanceLister interface {
		ListByHousehold(ctx context.Context, householdID household.HouseholdID, status tasksdomain.InstanceStatus, from, to time.Time) ([]*tasksdomain.TaskInstance, error)
	}
	recurringTaskGetter interface {
		Get(ctx context.Context, householdID household.HouseholdID, id tasksdomain.RecurringTaskID) (*tasksdomain.RecurringTask, error)
	}
	subscriptionLister interface {
		ListActiveByHousehold(ctx context.Context, householdID household.HouseholdID) ([]*subscriptionsdomain.Subscription, error)
	}
	memberLister interface {
		ListMembers(ctx context.Context, householdID household.HouseholdID) ([]*household.Member, error)
	}
)

// calendarTaskStatuses are the task-instance statuses shown on the calendar:
// upcoming (pending) and past-due (overdue). Done/skipped instances are omitted.
var calendarTaskStatuses = []tasksdomain.InstanceStatus{tasksdomain.StatusPending, tasksdomain.StatusOverdue}

// UnifiedCalendarService merges cached external events, task due dates, and
// subscription renewals into one ordered, member-colored timeline for a date
// range. It is read-only and deterministic given its injected dependencies.
type UnifiedCalendarService struct {
	events         externalEventLister
	tasks          taskInstanceLister
	recurringTasks recurringTaskGetter
	subs           subscriptionLister
	members        memberLister
	logger         *slog.Logger
}

// NewUnifiedCalendarService constructs the service with injected dependencies.
func NewUnifiedCalendarService(events externalEventLister, tasks taskInstanceLister, recurringTasks recurringTaskGetter, subs subscriptionLister, members memberLister, logger *slog.Logger) (*UnifiedCalendarService, error) {
	if events == nil || tasks == nil || recurringTasks == nil || subs == nil || members == nil {
		return nil, errors.New("calendar: NewUnifiedCalendarService requires non-nil repositories")
	}
	if logger == nil {
		return nil, errors.New("calendar: NewUnifiedCalendarService requires a non-nil logger")
	}
	return &UnifiedCalendarService{events: events, tasks: tasks, recurringTasks: recurringTasks, subs: subs, members: members, logger: logger}, nil
}

// List returns the household's unified calendar items whose start/date falls in
// [from, to], ordered by start time then title.
func (s *UnifiedCalendarService) List(ctx context.Context, householdID household.HouseholdID, from, to time.Time) ([]CalendarItem, error) {
	// An inverted range has no items; return early so a deterministic empty
	// result does not touch any repository.
	if to.Before(from) {
		return []CalendarItem{}, nil
	}

	colors, err := s.memberColors(ctx, householdID)
	if err != nil {
		return nil, err
	}

	items := make([]CalendarItem, 0)
	eventItems, err := s.eventItems(ctx, householdID, from, to)
	if err != nil {
		return nil, err
	}
	items = append(items, eventItems...)

	taskItems, err := s.taskItems(ctx, householdID, from, to, colors)
	if err != nil {
		return nil, err
	}
	items = append(items, taskItems...)

	renewalItems, err := s.renewalItems(ctx, householdID, from, to, colors)
	if err != nil {
		return nil, err
	}
	items = append(items, renewalItems...)

	sort.SliceStable(items, func(i, j int) bool {
		if !items[i].Start.Equal(items[j].Start) {
			return items[i].Start.Before(items[j].Start)
		}
		return items[i].Title < items[j].Title
	})
	return items, nil
}

// memberColors maps each household member id to its A-Hearth color string.
func (s *UnifiedCalendarService) memberColors(ctx context.Context, householdID household.HouseholdID) (map[household.MemberID]string, error) {
	members, err := s.members.ListMembers(ctx, householdID)
	if err != nil {
		return nil, fmt.Errorf("unified calendar: list members: %w", err)
	}
	colors := make(map[household.MemberID]string, len(members))
	for _, m := range members {
		colors[m.ID] = m.Color.String()
	}
	return colors, nil
}

func (s *UnifiedCalendarService) eventItems(ctx context.Context, householdID household.HouseholdID, from, to time.Time) ([]CalendarItem, error) {
	events, err := s.events.ListByHouseholdRange(ctx, householdID, from, to)
	if err != nil {
		return nil, fmt.Errorf("unified calendar: list events: %w", err)
	}
	items := make([]CalendarItem, 0, len(events))
	for _, e := range events {
		items = append(items, CalendarItem{
			Kind:     KindEvent,
			Title:    e.Title,
			Start:    e.StartsAt,
			End:      e.EndsAt,
			AllDay:   e.AllDay,
			SourceID: e.ID.String(),
		})
	}
	return items, nil
}

func (s *UnifiedCalendarService) taskItems(ctx context.Context, householdID household.HouseholdID, from, to time.Time, colors map[household.MemberID]string) ([]CalendarItem, error) {
	titles := make(map[tasksdomain.RecurringTaskID]string)
	items := make([]CalendarItem, 0)
	for _, status := range calendarTaskStatuses {
		instances, err := s.tasks.ListByHousehold(ctx, householdID, status, from, to)
		if err != nil {
			return nil, fmt.Errorf("unified calendar: list task instances (%s): %w", status, err)
		}
		for _, inst := range instances {
			title, err := s.taskTitle(ctx, householdID, inst.RecurringTaskID, titles)
			if err != nil {
				if errors.Is(err, tasksdomain.ErrTaskNotFound) {
					// A missing template should not drop the whole view; skip the item.
					s.logger.WarnContext(ctx, "unified calendar: skipping task with no template",
						"task_instance_id", inst.ID.String())
					continue
				}
				// Any other failure (e.g. a database error) is propagated.
				return nil, fmt.Errorf("unified calendar: resolve task title: %w", err)
			}
			items = append(items, CalendarItem{
				Kind:        KindTask,
				Title:       title,
				Start:       inst.DueOn,
				AllDay:      true,
				SourceID:    inst.ID.String(),
				MemberColor: colorFor(inst.AssigneeID, colors),
			})
		}
	}
	return items, nil
}

// taskTitle resolves a recurring task's title, caching by id within a call.
func (s *UnifiedCalendarService) taskTitle(ctx context.Context, householdID household.HouseholdID, id tasksdomain.RecurringTaskID, cache map[tasksdomain.RecurringTaskID]string) (string, error) {
	if title, ok := cache[id]; ok {
		return title, nil
	}
	task, err := s.recurringTasks.Get(ctx, householdID, id)
	if err != nil {
		return "", err
	}
	cache[id] = task.Title
	return task.Title, nil
}

func (s *UnifiedCalendarService) renewalItems(ctx context.Context, householdID household.HouseholdID, from, to time.Time, colors map[household.MemberID]string) ([]CalendarItem, error) {
	subs, err := s.subs.ListActiveByHousehold(ctx, householdID)
	if err != nil {
		return nil, fmt.Errorf("unified calendar: list subscriptions: %w", err)
	}
	items := make([]CalendarItem, 0)
	for _, sub := range subs {
		for _, occ := range projectRenewals(sub, from, to) {
			items = append(items, CalendarItem{
				Kind:        KindRenewal,
				Title:       sub.Name,
				Start:       occ,
				AllDay:      true,
				SourceID:    sub.ID.String(),
				MemberColor: colorFor(sub.PayerID, colors),
			})
		}
	}
	return items, nil
}

// projectRenewals returns the subscription's renewal occurrences in [from, to].
// A subscription whose cycle cannot be advanced (custom, or any unknown value)
// contributes none.
func projectRenewals(sub *subscriptionsdomain.Subscription, from, to time.Time) []time.Time {
	if _, err := subscriptionsapp.NextRenewal(sub.Cycle, sub.NextRenewalOn); err != nil {
		return nil
	}
	var occurrences []time.Time
	occ := sub.NextRenewalOn
	// Cap on the number of occurrences returned (not advancement steps), so a
	// NextRenewalOn that precedes the range still yields a full in-range set
	// rather than exhausting a step budget while advancing toward `from`.
	for len(occurrences) < maxRenewalOccurrences && !occ.After(to) {
		if !occ.Before(from) {
			occurrences = append(occurrences, occ)
		}
		next, err := subscriptionsapp.NextRenewal(sub.Cycle, occ)
		if err != nil {
			break // custom or unknown cycle: no further occurrences
		}
		occ = next
	}
	return occurrences
}

// colorFor returns the member's color for an optional attribution, or "" when
// unattributed or the member is not in the color map.
func colorFor(memberID *household.MemberID, colors map[household.MemberID]string) string {
	if memberID == nil {
		return ""
	}
	return colors[*memberID]
}
