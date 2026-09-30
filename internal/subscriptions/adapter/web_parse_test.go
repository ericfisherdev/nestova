package adapter

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func subscriptionForm(amount, currency string) *http.Request {
	form := url.Values{
		"name":            {"Streaming"},
		"amount":          {amount},
		"currency":        {currency},
		"cycle":           {"monthly"},
		"next_renewal_on": {"2026-10-01"},
	}
	r, _ := http.NewRequest(http.MethodPost, "/subscriptions", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

func TestParseSubscriptionInputAcceptsPlainDecimal(t *testing.T) {
	in, err := parseSubscriptionInput(subscriptionForm("9.99", "usd"))
	if err != nil {
		t.Fatalf("parseSubscriptionInput error = %v", err)
	}
	if in.Amount.Cents != 999 || in.Amount.Currency != "USD" {
		t.Errorf("Amount = %+v, want 999 USD", in.Amount)
	}
}

func TestParseSubscriptionInputRefusalsReadAsSentences(t *testing.T) {
	cases := []struct{ name, amount, currency string }{
		{"exponent", "1e10", "USD"},
		{"sub-cent", "9.999", "USD"},
		{"negative", "-1", "USD"},
		{"bad currency", "9.99", "usdx"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseSubscriptionInput(subscriptionForm(tc.amount, tc.currency))
			if err == nil {
				t.Fatal("parseSubscriptionInput error = nil, want refusal")
			}
			for _, leak := range []string{"household:", "invalid money", "got \""} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("error %q leaks Go error text %q", err, leak)
				}
			}
		})
	}
}
