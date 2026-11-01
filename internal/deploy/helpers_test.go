package deploy

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

type httpServer struct{ port string }

// startHTTP serves one fixed status code on a random local port until the test ends.
func startHTTP(t *testing.T, status int) httpServer {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
	t.Cleanup(srv.Close)
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := strconv.Atoi(port); err != nil {
		t.Fatal(err)
	}
	return httpServer{port: port}
}
