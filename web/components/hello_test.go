package components_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ericfisherdev/nestova/web/components"
)

// TestHelloRenders verifies that templ-generated Go compiles, renders its input
// through the templ runtime, and HTML-escapes interpolated values.
func TestHelloRenders(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "plain name renders verbatim",
			input: "Nestova",
			want:  "<h1>Hello, Nestova!</h1>",
		},
		{
			name:  "html in name is escaped",
			input: "<script>alert(1)</script>",
			want:  "<h1>Hello, &lt;script&gt;alert(1)&lt;/script&gt;!</h1>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var sb strings.Builder
			if err := components.Hello(tt.input).Render(context.Background(), &sb); err != nil {
				t.Fatalf("Hello.Render returned error: %v", err)
			}
			if got := sb.String(); got != tt.want {
				t.Errorf("rendered output = %q, want %q", got, tt.want)
			}
		})
	}
}
