package setup

import "strings"

// ResolveToken returns the token that gates the first-run wizard. The
// operator-supplied value from lookup(SetupTokenEnv) wins when it is non-blank;
// otherwise a fresh token is generated and generated is true so the caller can
// announce it. The wizard is never left without a token.
//
// Errors: only the generator's error (a crypto/rand failure), returned as is.
// The caller must treat it as fatal rather than serve an open wizard.
func ResolveToken(lookup func(string) string, generate func() (string, error)) (token string, generated bool, err error) {
	if supplied := strings.TrimSpace(lookup(SetupTokenEnv)); supplied != "" {
		return supplied, false, nil
	}
	token, err = generate()
	if err != nil {
		return "", false, err
	}
	return token, true, nil
}
