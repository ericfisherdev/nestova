package httpserver

import (
	"net/http"
	"strings"
)

// nulByte is the character Postgres text columns cannot store: the driver
// surfaces it as SQLSTATE 22021, which a handler would report as a 500.
const nulByte = "\x00"

// refuseNULBytes rejects a form or query submission carrying a NUL byte in any
// value with 422 Unprocessable Entity, so no handler forwards one to the
// database (NES-195). Doing it once at the transport edge covers every text
// field, including ones added later, instead of each domain re-checking.
//
// Only urlencoded bodies and the query string are inspected: r.ParseForm leaves
// multipart bodies untouched (photo uploads carry binary parts by design), and
// a parse failure is passed through so the handler words its own 400.
func refuseNULBytes(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ParseForm() == nil && formHasNULByte(r) {
			http.Error(w, "text must not contain null characters", http.StatusUnprocessableEntity)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func formHasNULByte(r *http.Request) bool {
	for key, values := range r.Form {
		if strings.Contains(key, nulByte) {
			return true
		}
		for _, value := range values {
			if strings.Contains(value, nulByte) {
				return true
			}
		}
	}
	return false
}
