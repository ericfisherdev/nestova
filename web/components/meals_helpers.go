package components

import (
	"encoding/json"
	"strconv"
	"strings"
)

// fmtServings renders a servings count for a number input, defaulting to 1.
func fmtServings(n int) string {
	if n < 1 {
		n = 1
	}
	return strconv.Itoa(n)
}

// joinNames renders a list of ingredient names for the finder's "missing" line.
func joinNames(names []string) string {
	return strings.Join(names, ", ")
}

// mealRecipeFormState renders the Alpine x-data initializer for a recipe form's
// repeatable ingredient lines as a JSON object literal (e.g. {"lines":[...]}).
// JSON-encoding keeps a hostile sticky value from breaking out of the x-data
// expression (Alpine expression injection); templ also HTML-escapes it at render
// time. An empty set seeds a single blank line so the form always has one row.
func mealRecipeFormState(lines []MealEditLineView) string {
	if len(lines) == 0 {
		lines = []MealEditLineView{{Unit: "count"}}
	}
	out := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		out = append(out, map[string]any{
			"name":     line.Name,
			"amount":   line.Amount,
			"unit":     line.Unit,
			"optional": line.Optional,
		})
	}
	b, err := json.Marshal(map[string]any{"lines": out})
	if err != nil {
		return `{"lines":[{"name":"","amount":"","unit":"count","optional":false}]}`
	}
	return string(b)
}
