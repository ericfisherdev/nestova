// Package components holds the Go Templ view components and their small,
// hand-written rendering helpers for the Nestova web UI.
package components

import "encoding/json"

// alpineTaskState renders the Alpine x-data initializer for the new-task form as
// a JSON object literal, e.g. {"freq":"weekly","policy":""}. The sticky freq and
// policy values originate from user input on a 422 re-render; JSON-encoding them
// keeps a hostile value from breaking out of the x-data expression (Alpine
// expression injection). The result is also HTML-escaped by templ at render time.
func alpineTaskState(freq, policy string) string {
	b, err := json.Marshal(map[string]string{"freq": freq, "policy": policy})
	if err != nil {
		return "{}"
	}
	return string(b)
}
