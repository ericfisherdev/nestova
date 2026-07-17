package domain_test

import (
	"errors"
	"testing"

	"github.com/ericfisherdev/nestova/internal/deeplink/domain"
)

func TestAction_Validate(t *testing.T) {
	tests := []struct {
		name    string
		action  domain.Action
		wantErr error
	}{
		{"claim task", domain.ActionClaimTask, nil},
		{"complete task", domain.ActionCompleteTask, nil},
		{"add chore", domain.ActionAddChore, nil},
		{"redeem reward", domain.ActionRedeemReward, nil},
		{"unknown", domain.Action("delete-household"), domain.ErrUnknownAction},
		{"empty", domain.Action(""), domain.ErrUnknownAction},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.action.Validate()
			if tt.wantErr == nil && err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("Validate() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestAction_RequiresID(t *testing.T) {
	tests := []struct {
		action domain.Action
		want   bool
	}{
		{domain.ActionClaimTask, true},
		{domain.ActionCompleteTask, true},
		{domain.ActionRedeemReward, true},
		{domain.ActionAddChore, false},
	}
	for _, tt := range tests {
		if got := tt.action.RequiresID(); got != tt.want {
			t.Errorf("%s.RequiresID() = %v, want %v", tt.action, got, tt.want)
		}
	}
}

func TestAction_Path(t *testing.T) {
	tests := []struct {
		name    string
		action  domain.Action
		id      string
		want    string
		wantErr error
	}{
		{"claim task with id", domain.ActionClaimTask, "abc-123", "/go/claim-task/abc-123", nil},
		{"complete task with id", domain.ActionCompleteTask, "abc-123", "/go/complete-task/abc-123", nil},
		{"redeem reward with id", domain.ActionRedeemReward, "reward-1", "/go/redeem-reward/reward-1", nil},
		{"add chore with no id", domain.ActionAddChore, "", "/go/add-chore", nil},
		{"claim task missing id", domain.ActionClaimTask, "", "", domain.ErrMissingID},
		{"add chore with unexpected id", domain.ActionAddChore, "abc-123", "", domain.ErrMissingID},
		{"unknown action", domain.Action("nope"), "abc-123", "", domain.ErrUnknownAction},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.action.Path(tt.id)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Path(%q) error = %v, want %v", tt.id, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Path(%q) unexpected error: %v", tt.id, err)
			}
			if got != tt.want {
				t.Errorf("Path(%q) = %q, want %q", tt.id, got, tt.want)
			}
		})
	}
}
