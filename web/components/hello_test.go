package components_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ericfisherdev/nestova/web/components"
)

// TestHelloRendersName verifies that templ-generated Go compiles and that a
// component renders its input through the templ runtime.
func TestHelloRendersName(t *testing.T) {
	var sb strings.Builder
	if err := components.Hello("Nestova").Render(context.Background(), &sb); err != nil {
		t.Fatalf("Hello.Render returned error: %v", err)
	}

	if got, want := sb.String(), "Hello, Nestova!"; !strings.Contains(got, want) {
		t.Errorf("rendered output = %q, want it to contain %q", got, want)
	}
}
