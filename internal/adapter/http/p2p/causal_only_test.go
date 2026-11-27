package p2p

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLegacyTransfersAreNotExposed(t *testing.T) {
	h := NewHttpHandler("", &countingService{}, nil)
	for _, path := range []string{"/copy", "/transfer"} {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		w := httptest.NewRecorder()
		h.server.Handler.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s: status=%d", path, w.Code)
		}
	}
}
