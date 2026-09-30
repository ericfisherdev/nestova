package httpserver

import (
	"mime"
	"net/http"
	"strings"
)

// nulByte is the character Postgres text columns cannot store: the driver
// surfaces it as SQLSTATE 22021, which a handler would report as a 500.
const nulByte = "\x00"

// maxFormBodyBytes caps the urlencoded body refuseNULBytes parses. No form in
// the app comes near it, and it stops a spoofed Content-Type on a small-body
// route (the WebAuthn finish handlers cap theirs at 64 KiB) from costing the
// 10 MiB net/http would otherwise read before the handler's own limit applies.
const maxFormBodyBytes = 1 << 20

// refuseNULBytes rejects a form or query submission carrying a NUL byte in any
// value with 422 Unprocessable Entity, so no handler forwards one to the
// database (NES-195). Doing it once at the transport edge covers every text
// field, including ones added later, instead of each domain re-checking.
//
// A submission that does not parse is refused here with 400: ParseForm keeps
// the values it managed to read, so passing it on would let a handler's own
// ParseForm return nil and read a NUL that this check never saw.
//
// Only urlencoded bodies and the query string are inspected; multipart and
// JSON bodies are not read here, so their text fields are checked where they
// are consumed (Photo.Validate, ValidateNickname).
func refuseNULBytes(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isURLEncoded(r) {
			r.Body = http.MaxBytesReader(w, r.Body, maxFormBodyBytes)
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if formHasNULByte(r) {
			http.Error(w, "text must not contain null characters", http.StatusUnprocessableEntity)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isURLEncoded(r *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mediaType == "application/x-www-form-urlencoded"
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
