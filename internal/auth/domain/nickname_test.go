package domain_test

import (
	"errors"
	"strings"
	"testing"

	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
)

func TestValidateNickname(t *testing.T) {
	t.Parallel()

	tests := []struct {
		label    string
		nickname string
		want     error
	}{
		{"blank falls back to the default", "", nil},
		{"at the limit", strings.Repeat("a", authdomain.MaxNicknameLength), nil},
		{"multibyte at the limit", strings.Repeat("家", authdomain.MaxNicknameLength), nil},
		{"one over", strings.Repeat("a", authdomain.MaxNicknameLength+1), authdomain.ErrNicknameTooLong},
		{"multibyte one over", strings.Repeat("家", authdomain.MaxNicknameLength+1), authdomain.ErrNicknameTooLong},
		{"far over", strings.Repeat("n", 10_000), authdomain.ErrNicknameTooLong},
	}
	for _, tc := range tests {
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()
			if err := authdomain.ValidateNickname(tc.nickname); !errors.Is(err, tc.want) {
				t.Errorf("ValidateNickname() = %v, want %v", err, tc.want)
			}
		})
	}
}
