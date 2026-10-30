package clitest

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/entireio/entire-brain/factmerge"
)

// WebhookRecorder is a fake endpoint that remembers what it was sent.
type WebhookRecorder struct {
	Mu       sync.Mutex
	Requests []WebhookRecordedRequest
	Status   int
	Redirect string
}

type WebhookRecordedRequest struct {
	Body    []byte
	Headers http.Header
}

func (r *WebhookRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body := &bytes.Buffer{}
	_, _ = body.ReadFrom(req.Body)
	r.Mu.Lock()
	r.Requests = append(r.Requests, WebhookRecordedRequest{Body: body.Bytes(), Headers: req.Header.Clone()})
	redirect, status := r.Redirect, r.Status
	r.Mu.Unlock()
	if redirect != "" {
		// 307 is the interesting case: unlike 302 it preserves the method and
		// body, so a client that follows it re-delivers the payload.
		http.Redirect(w, req, redirect, http.StatusTemporaryRedirect)
		return
	}
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
}

func (r *WebhookRecorder) Count() int {
	r.Mu.Lock()
	defer r.Mu.Unlock()
	return len(r.Requests)
}

func (r *WebhookRecorder) Last(t *testing.T) WebhookRecordedRequest {
	t.Helper()
	r.Mu.Lock()
	defer r.Mu.Unlock()
	if len(r.Requests) == 0 {
		t.Fatal("the endpoint was never called")
	}
	return r.Requests[len(r.Requests)-1]
}

// WebhookEndpointServer starts a fake endpoint and points the brain at it.
// The URL environment variable's name lives in command/webhook, which this
// package cannot import (webhook's tests import clitest), so callers pass it.
func WebhookEndpointServer(t *testing.T, urlEnv string) *WebhookRecorder {
	t.Helper()
	rec := &WebhookRecorder{}
	server := httptest.NewServer(rec)
	t.Cleanup(server.Close)
	t.Setenv(urlEnv, server.URL)
	return rec
}

func SampleWebhookFact() factmerge.Record {
	return factmerge.Record{
		ID:    "fact-1",
		Kind:  "invariant",
		Paths: []string{"architecture.storage.invariant"},
		Text:  "Refs are written through a Postgres compare-and-swap.",
	}
}

func WebhookTestNow() time.Time { return time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC) }
