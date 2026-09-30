package adapter

import (
	"errors"
	"testing"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

func TestParseQuantityRefusals(t *testing.T) {
	for _, amount := range []string{"1e10", "1e300", "1000000001", "+5", "1,000", ""} {
		if _, err := parseQuantity(amount, "count"); !errors.Is(err, household.ErrInvalidQuantity) {
			t.Errorf("parseQuantity(%q) error = %v, want ErrInvalidQuantity", amount, err)
		}
	}
}

func TestParseQuantityAcceptsPlainDecimal(t *testing.T) {
	q, err := parseQuantity(" 1.5 ", "kg")
	if err != nil || q.Amount != 1.5 || q.Unit != household.UnitKilogram {
		t.Errorf("parseQuantity(1.5, kg) = %+v, %v", q, err)
	}
}
