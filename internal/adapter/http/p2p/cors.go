package p2p

import (
	"net/http"

	"github.com/indexus/go-indexus-core/internal/wire"
)

// CORS for the browser SDKs, written to answer exactly as rs/cors v1.11.1
// did with AllowedOrigins "*", AllowedHeaders "*", no credentials and the
// methods below (cors_test.go holds its recorded answers).
//
// Browser SDKs (axios arraybuffer /sets) preflight with Accept besides
// Authorization, so every requested header is allowed: a narrow list would
// omit ACAO on OPTIONS, which Chrome reports as "blocked by CORS".
// Credentials cannot be allowed with origin "*"; Bearer auth needs no cookies.
// Ingress hint response headers must be exposed or axios cannot read them.
var (
	corsPreflightVaryValue = []string{"Origin, Access-Control-Request-Method, Access-Control-Request-Headers"}
	corsVaryOrigin         = []string{"Origin"}
	corsAllOrigins         = []string{"*"}
	corsExposeValue        = []string{"X-Indexus-Ingress-Name, X-Indexus-Ingress-Ip, X-Indexus-Ingress-Port, " +
		wire.HeaderDecodedBytes + ", " + wire.HeaderSHA256 + ", " + wire.HeaderStoredEncoding + ", Content-Length, Server-Timing"}
)

// corsMethodAllowed: OPTIONS always; HEAD and PATCH are not in the list.
func corsMethodAllowed(m string) bool {
	switch m {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions:
		return true
	}
	return false
}

func withCORS(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers := w.Header()
		origin := r.Header.Get("Origin")
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			// Preflights stop here: the handlers behind would ask for auth.
			headers["Vary"] = append(headers["Vary"], corsPreflightVaryValue...)
			if origin != "" && corsMethodAllowed(r.Header.Get("Access-Control-Request-Method")) {
				headers["Access-Control-Allow-Origin"] = corsAllOrigins
				headers["Access-Control-Allow-Methods"] = r.Header["Access-Control-Request-Method"]
				if reqHeaders := r.Header["Access-Control-Request-Headers"]; len(reqHeaders) > 0 && reqHeaders[0] != "" {
					headers["Access-Control-Allow-Headers"] = reqHeaders
				}
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		headers["Vary"] = append(headers["Vary"], corsVaryOrigin...)
		if origin != "" && corsMethodAllowed(r.Method) {
			headers["Access-Control-Allow-Origin"] = corsAllOrigins
			headers["Access-Control-Expose-Headers"] = corsExposeValue
		}
		h.ServeHTTP(w, r)
	})
}
