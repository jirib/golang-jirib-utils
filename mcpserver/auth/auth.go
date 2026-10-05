// Package auth provides HTTP bearer token authentication middleware.
package auth

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// BearerMiddleware validates the Authorization header bearer token using constant-time comparison.
func BearerMiddleware(token, realm string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		auth := r.Header.Get("Authorization")
		presented, ok := strings.CutPrefix(auth, prefix)
		if !ok || subtle.ConstantTimeCompare([]byte(presented), []byte(token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+realm+`"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
