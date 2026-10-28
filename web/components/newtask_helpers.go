// Package components holds the Go Templ view components and their small,
// hand-written rendering helpers for the Nestova web UI.
package components

import "encoding/json"

// alpineTaskState renders the new-task form's initial taskForm state as a JSON
// object for its data-state attribute, e.g. {"freq":"weekly","policy":""}. The
// sticky freq and policy values originate from user input on a 422 re-render;
// task-form.js JSON.parses the attribute and never evaluates it as an Alpine
// expression, so a hostile value cannot become script. The result is also
// HTML-escaped by templ at render time.
func alpineTaskState(freq, policy string) string {
	b, err := json.Marshal(map[string]string{"freq": freq, "policy": policy})
	if err != nil {
		return "{}"
	}
	return string(b)
}
