package adapter

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ericfisherdev/nestova/internal/subscriptions/domain"
)

func TestParseAmountCentsRefusesAboveCeiling(t *testing.T) {
	for _, in := range []string{"1000000000.01", "50000000000000000", "92233720368547758"} {
		if _, err := parseAmountCents(in); !errors.Is(err, errAmountTooLarge) {
			t.Errorf("parseAmountCents(%q) error = %v, want errAmountTooLarge", in, err)
		}
	}
}

func TestParseAmountCentsAcceptsCeiling(t *testing.T) {
	got, err := parseAmountCents("1000000000")
	if err != nil || got != domain.MaxAmountCents {
		t.Fatalf("parseAmountCents(ceiling) = %d, %v; want %d, nil", got, err, domain.MaxAmountCents)
	}
}

func TestRespondInvalidInputAmountTooLarge(t *testing.T) {
	_, err := parseAmountCents("1000000000.01")
	rec := httptest.NewRecorder()
	respondInvalidInput(rec, err)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "subscriptions:") {
		t.Fatalf("body leaks the Go sentinel: %q", rec.Body.String())
	}
}

func TestRespondInvalidInputOtherErrorIs400(t *testing.T) {
	rec := httptest.NewRecorder()
	respondInvalidInput(rec, errors.New("invalid payer"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}
