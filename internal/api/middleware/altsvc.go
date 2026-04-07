package middleware

import (
	"fmt"
	"net/http"
)

// AltSvc returns middleware that advertises HTTP/3 support via the Alt-Svc
// response header. It should only be applied when the HTTP/3 listener is
// actually running.
func AltSvc(port int) func(http.Handler) http.Handler {
	headerValue := fmt.Sprintf(`h3=":%d"; ma=86400`, port)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Alt-Svc", headerValue)
			next.ServeHTTP(w, r)
		})
	}
}
