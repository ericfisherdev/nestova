package domain

import (
	"errors"
	"fmt"
)

// Action identifies which kiosk action a QR deep link performs. It is a
// typed enum (rather than a bare string) so an unrecognized value is rejected
// by [Action.Validate] at the boundary instead of silently reaching a switch
// with no matching case.
type Action string

const (
	// ActionClaimTask deep-links to claiming a specific pending/overdue chore
	// instance (path carries the TaskInstanceID).
	ActionClaimTask Action = "claim-task"
	// ActionCompleteTask deep-links to completing a specific chore instance
	// (path carries the TaskInstanceID).
	ActionCompleteTask Action = "complete-task"
	// ActionAddChore deep-links to the new-recurring-task form. It carries no
	// resource id — see [Action.RequiresID] — since it targets a form, not an
	// existing record.
	ActionAddChore Action = "add-chore"
	// ActionRedeemReward deep-links to redeeming a specific reward (path
	// carries the RewardID).
	ActionRedeemReward Action = "redeem-reward"
)

// ErrUnknownAction is returned by [Action.Validate] when the action segment of
// a /go/ deep-link path is not one of the recognized constants.
var ErrUnknownAction = errors.New("deeplink: unknown action")

// Validate reports whether a is one of the recognized deep-link actions,
// returning [ErrUnknownAction] wrapped with the offending value otherwise.
func (a Action) Validate() error {
	switch a {
	case ActionClaimTask, ActionCompleteTask, ActionAddChore, ActionRedeemReward:
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrUnknownAction, string(a))
	}
}

// RequiresID reports whether a's route carries a resource id path segment.
// Every action requires one except [ActionAddChore], which deep-links to a
// form rather than acting on an existing record.
func (a Action) RequiresID() bool {
	return a != ActionAddChore
}

// ErrMissingID is returned by [Action.Path] when a requires an id (per
// [Action.RequiresID]) but id is empty, and when id is non-empty for an
// action that requires none.
var ErrMissingID = errors.New("deeplink: action/id mismatch")

// Path builds the canonical, unsigned /go/ route path for a and id — the
// exact string the deeplink/app package's Signer computes its signature over.
// It returns [ErrMissingID] when id is empty for an action that requires one,
// or non-empty for one that does not (currently only [ActionAddChore]), so a
// caller can never accidentally sign or verify a path shape the router would
// not actually serve.
func (a Action) Path(id string) (string, error) {
	if err := a.Validate(); err != nil {
		return "", err
	}
	if a.RequiresID() {
		if id == "" {
			return "", fmt.Errorf("%w: %q requires a non-empty id", ErrMissingID, string(a))
		}
		return "/go/" + string(a) + "/" + id, nil
	}
	if id != "" {
		return "", fmt.Errorf("%w: %q does not take an id, got %q", ErrMissingID, string(a), id)
	}
	return "/go/" + string(a), nil
}
