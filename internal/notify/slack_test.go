package notify

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSend(t *testing.T) {
	var got map[string]string
	var ctype string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctype = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
	}))
	defer srv.Close()
	if err := (Slack{URL: srv.URL}).Send(t.Context(), `:rocket: "quoted" é`); err != nil {
		t.Fatal(err)
	}
	if got["text"] != `:rocket: "quoted" é` || ctype != "application/json" {
		t.Errorf("payload %v, content type %q", got, ctype)
	}
}

func TestSendWithoutURLIsSilent(t *testing.T) {
	if err := (Slack{}).Send(t.Context(), "x"); err != nil {
		t.Error(err)
	}
}

func TestErrorsNeverExposeTheWebhookURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }))
	url := srv.URL + "/services/T000/B000/SECRETTOKEN"
	err := (Slack{URL: url}).Send(t.Context(), "x")
	if err == nil || strings.Contains(err.Error(), "SECRETTOKEN") {
		t.Errorf("HTTP error: %v", err)
	}
	srv.Close()
	err = (Slack{URL: url}).Send(t.Context(), "x")
	if err == nil || strings.Contains(err.Error(), "SECRETTOKEN") {
		t.Errorf("network error must not leak the URL: %v", err)
	}
	if err := (Slack{URL: "://bad SECRETTOKEN"}).Send(t.Context(), "x"); err == nil || strings.Contains(err.Error(), "SECRETTOKEN") {
		t.Errorf("invalid URL error: %v", err)
	}
}
