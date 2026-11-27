package p2p

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// corsCase is one request against withCORS. The want values were recorded
// from github.com/rs/cors v1.11.1 with the node's options (golden), so the
// in-repo middleware must answer byte for byte as rs/cors did.
type corsCase struct {
	name    string
	method  string
	headers [][2]string
	status  int
	want    http.Header
	body    string
}

const corsExpose = "X-Indexus-Ingress-Name, X-Indexus-Ingress-Ip, X-Indexus-Ingress-Port, X-Indexus-Decoded-Bytes, X-Indexus-Sha256, X-Indexus-Stored-Encoding, Content-Length, Server-Timing"

const corsPreflightVary = "Origin, Access-Control-Request-Method, Access-Control-Request-Headers"

var corsCases = []corsCase{
	{name: "preflight GET with headers", method: "OPTIONS", headers: [][2]string{{"Origin", "https://app.example"}, {"Access-Control-Request-Method", "GET"}, {"Access-Control-Request-Headers", "authorization,accept"}},
		status: 204, want: http.Header{"Vary": {corsPreflightVary}, "Access-Control-Allow-Origin": {"*"}, "Access-Control-Allow-Methods": {"GET"}, "Access-Control-Allow-Headers": {"authorization,accept"}}},
	{name: "preflight PUT without headers", method: "OPTIONS", headers: [][2]string{{"Origin", "https://app.example"}, {"Access-Control-Request-Method", "PUT"}},
		status: 204, want: http.Header{"Vary": {corsPreflightVary}, "Access-Control-Allow-Origin": {"*"}, "Access-Control-Allow-Methods": {"PUT"}}},
	{name: "preflight DELETE", method: "OPTIONS", headers: [][2]string{{"Origin", "http://localhost:5173"}, {"Access-Control-Request-Method", "DELETE"}, {"Access-Control-Request-Headers", "content-type"}},
		status: 204, want: http.Header{"Vary": {corsPreflightVary}, "Access-Control-Allow-Origin": {"*"}, "Access-Control-Allow-Methods": {"DELETE"}, "Access-Control-Allow-Headers": {"content-type"}}},
	{name: "preflight OPTIONS method", method: "OPTIONS", headers: [][2]string{{"Origin", "https://app.example"}, {"Access-Control-Request-Method", "OPTIONS"}},
		status: 204, want: http.Header{"Vary": {corsPreflightVary}, "Access-Control-Allow-Origin": {"*"}, "Access-Control-Allow-Methods": {"OPTIONS"}}},
	{name: "preflight disallowed method PATCH", method: "OPTIONS", headers: [][2]string{{"Origin", "https://app.example"}, {"Access-Control-Request-Method", "PATCH"}, {"Access-Control-Request-Headers", "authorization"}},
		status: 204, want: http.Header{"Vary": {corsPreflightVary}}},
	{name: "preflight disallowed method HEAD", method: "OPTIONS", headers: [][2]string{{"Origin", "https://app.example"}, {"Access-Control-Request-Method", "HEAD"}},
		status: 204, want: http.Header{"Vary": {corsPreflightVary}}},
	{name: "preflight lowercase method", method: "OPTIONS", headers: [][2]string{{"Origin", "https://app.example"}, {"Access-Control-Request-Method", "get"}},
		status: 204, want: http.Header{"Vary": {corsPreflightVary}}},
	{name: "preflight without origin", method: "OPTIONS", headers: [][2]string{{"Access-Control-Request-Method", "GET"}},
		status: 204, want: http.Header{"Vary": {corsPreflightVary}}},
	{name: "preflight empty request headers", method: "OPTIONS", headers: [][2]string{{"Origin", "https://app.example"}, {"Access-Control-Request-Method", "POST"}, {"Access-Control-Request-Headers", ""}},
		status: 204, want: http.Header{"Vary": {corsPreflightVary}, "Access-Control-Allow-Origin": {"*"}, "Access-Control-Allow-Methods": {"POST"}}},
	{name: "preflight split request headers", method: "OPTIONS", headers: [][2]string{{"Origin", "https://app.example"}, {"Access-Control-Request-Method", "POST"}, {"Access-Control-Request-Headers", "authorization"}, {"Access-Control-Request-Headers", "x-custom"}},
		status: 204, want: http.Header{"Vary": {corsPreflightVary}, "Access-Control-Allow-Origin": {"*"}, "Access-Control-Allow-Methods": {"POST"}, "Access-Control-Allow-Headers": {"authorization", "x-custom"}}},
	{name: "preflight private network", method: "OPTIONS", headers: [][2]string{{"Origin", "https://app.example"}, {"Access-Control-Request-Method", "GET"}, {"Access-Control-Request-Private-Network", "true"}},
		status: 204, want: http.Header{"Vary": {corsPreflightVary}, "Access-Control-Allow-Origin": {"*"}, "Access-Control-Allow-Methods": {"GET"}}},
	{name: "preflight null origin", method: "OPTIONS", headers: [][2]string{{"Origin", "null"}, {"Access-Control-Request-Method", "GET"}},
		status: 204, want: http.Header{"Vary": {corsPreflightVary}, "Access-Control-Allow-Origin": {"*"}, "Access-Control-Allow-Methods": {"GET"}}},
	{name: "OPTIONS without request method is actual", method: "OPTIONS", headers: [][2]string{{"Origin", "https://app.example"}},
		status: 200, body: "ok", want: http.Header{"Vary": {"Origin", "Accept-Encoding"}, "Access-Control-Allow-Origin": {"*"}, "Access-Control-Expose-Headers": {corsExpose}, "X-Inner": {"1"}, "Content-Type": {"text/plain; charset=utf-8"}}},
	{name: "simple GET", method: "GET", headers: [][2]string{{"Origin", "https://app.example"}},
		status: 200, body: "ok", want: http.Header{"Vary": {"Origin", "Accept-Encoding"}, "Access-Control-Allow-Origin": {"*"}, "Access-Control-Expose-Headers": {corsExpose}, "X-Inner": {"1"}, "Content-Type": {"text/plain; charset=utf-8"}}},
	{name: "simple POST", method: "POST", headers: [][2]string{{"Origin", "https://app.example"}, {"Content-Type", "application/json"}},
		status: 200, body: "ok", want: http.Header{"Vary": {"Origin", "Accept-Encoding"}, "Access-Control-Allow-Origin": {"*"}, "Access-Control-Expose-Headers": {corsExpose}, "X-Inner": {"1"}, "Content-Type": {"text/plain; charset=utf-8"}}},
	{name: "GET with credentials", method: "GET", headers: [][2]string{{"Origin", "https://app.example"}, {"Cookie", "a=b"}, {"Authorization", "Bearer x"}},
		status: 200, body: "ok", want: http.Header{"Vary": {"Origin", "Accept-Encoding"}, "Access-Control-Allow-Origin": {"*"}, "Access-Control-Expose-Headers": {corsExpose}, "X-Inner": {"1"}, "Content-Type": {"text/plain; charset=utf-8"}}},
	{name: "GET without origin", method: "GET",
		status: 200, body: "ok", want: http.Header{"Vary": {"Origin", "Accept-Encoding"}, "X-Inner": {"1"}, "Content-Type": {"text/plain; charset=utf-8"}}},
	{name: "HEAD not in allowed methods", method: "HEAD", headers: [][2]string{{"Origin", "https://app.example"}},
		status: 200, body: "ok", want: http.Header{"Vary": {"Origin", "Accept-Encoding"}, "X-Inner": {"1"}, "Content-Type": {"text/plain; charset=utf-8"}}},
	{name: "PATCH not in allowed methods", method: "PATCH", headers: [][2]string{{"Origin", "https://app.example"}},
		status: 200, body: "ok", want: http.Header{"Vary": {"Origin", "Accept-Encoding"}, "X-Inner": {"1"}, "Content-Type": {"text/plain; charset=utf-8"}}},
}

func TestCORSGolden(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Inner", "1")
		w.Header().Add("Vary", "Accept-Encoding")
		w.Write([]byte("ok"))
	})
	h := withCORS(inner)
	for _, c := range corsCases {
		r := httptest.NewRequest(c.method, "/sets", nil)
		for _, kv := range c.headers {
			r.Header.Add(kv[0], kv[1])
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.status {
			t.Errorf("%s: status %d, want %d", c.name, w.Code, c.status)
		}
		if !reflect.DeepEqual(w.Header(), c.want) {
			t.Errorf("%s: headers\n got %#v\nwant %#v", c.name, w.Header(), c.want)
		}
		if w.Body.String() != c.body {
			t.Errorf("%s: body %q, want %q", c.name, w.Body.String(), c.body)
		}
	}
}

// The ready gate sits inside CORS: a 503 while the cluster starts still
// carries the CORS headers, so a browser can read the error.
func TestCORSWrapsReadyGate(t *testing.T) {
	h := NewHttpHandler("", nil, nil)
	h.Ready = func() bool { return false }
	r := httptest.NewRequest("GET", "/sets", nil)
	r.Header.Set("Origin", "https://app.example")
	w := httptest.NewRecorder()
	h.server.Handler.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Access-Control-Allow-Origin") != "*" || w.Header().Get("Access-Control-Expose-Headers") != corsExpose {
		t.Fatalf("status %d headers %#v", w.Code, w.Header())
	}
	r = httptest.NewRequest("OPTIONS", "/sets", nil)
	r.Header.Set("Origin", "https://app.example")
	r.Header.Set("Access-Control-Request-Method", "GET")
	w = httptest.NewRecorder()
	h.server.Handler.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent || w.Body.Len() != 0 || w.Header().Get("Access-Control-Allow-Methods") != "GET" {
		t.Fatalf("preflight status %d headers %#v", w.Code, w.Header())
	}
}
