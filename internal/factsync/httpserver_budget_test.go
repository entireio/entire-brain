package factsync

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestRetryBudgetDoesNotMultiplyTheRequestBound is the composition assertion.
//
// syncRequestTimeout bounds ONE http.Client.Do. do() may issue up to
// 1+transientDialRetries of them, so before the operation deadline existed the real
// worst case a caller could experience was
//
//	3 x syncRequestTimeout + 2 x transientDialRetryDelay
//
// i.e. three times the bound the code documented as "one fact-set sync request end
// to end". A caller reading syncRequestTimeout got a number that was wrong by 3x.
//
// The test drives the retry path for real -- every attempt fails with the
// EADDRNOTAVAIL-shaped dial error do() retries on -- and asserts that the whole call
// is bounded by syncOperationTimeout, NOT by attempts x syncRequestTimeout.
func TestRetryBudgetDoesNotMultiplyTheRequestBound(t *testing.T) {
	// A per-attempt bound far larger than the operation budget: if the operation
	// deadline is missing or is applied per attempt, this test blows its own budget.
	restoreReq, restoreOp := syncRequestTimeout, syncOperationTimeout
	syncRequestTimeout = time.Hour
	syncOperationTimeout = 400 * time.Millisecond
	t.Cleanup(func() { syncRequestTimeout, syncOperationTimeout = restoreReq, restoreOp })

	h := &HTTPServer{BaseURL: "https://example.invalid", Token: "t", Client: &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			// Block until the request context is done, then report the retryable
			// local-dial failure. Without an operation-wide deadline the context
			// never fires and this hangs for a full syncRequestTimeout per attempt.
			<-r.Context().Done()
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errAddrNotAvail{}}
		}),
	}}

	req, err := h.newRequest(context.Background(), http.MethodGet, "/api/v1/x", nil)
	if err != nil {
		t.Fatalf("newRequest: %v", err)
	}
	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		resp, _ := h.do(req)
		if resp != nil {
			resp.Body.Close()
		}
		done <- time.Since(start)
	}()
	select {
	case elapsed := <-done:
		// The whole call, retries included, must respect the operation budget --
		// with slack for scheduling, but nowhere near a second attempt's worth.
		if elapsed > 5*time.Second {
			t.Fatalf("do() took %v; the retry budget is multiplying the request bound instead of sharing one operation budget", elapsed)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("do() never returned: the retry loop has no overall ceiling, so it stacks syncRequestTimeout per attempt")
	}
}

// TestOperationCeilingIsTheStatedArithmetic pins the documented numbers so the
// comment and the constants cannot drift apart.
func TestOperationCeilingIsTheStatedArithmetic(t *testing.T) {
	attempts := 1 + transientDialRetries
	naiveWorstCase := time.Duration(attempts)*syncRequestTimeout + time.Duration(transientDialRetries)*transientDialRetryDelay
	if syncOperationTimeout >= naiveWorstCase {
		t.Fatalf("syncOperationTimeout (%v) does not cap anything: the unbounded stack is %d x %v + %d x %v = %v",
			syncOperationTimeout, attempts, syncRequestTimeout, transientDialRetries, transientDialRetryDelay, naiveWorstCase)
	}
	if syncOperationTimeout < syncRequestTimeout {
		t.Fatalf("syncOperationTimeout (%v) is below syncRequestTimeout (%v): a single healthy slow request could not finish", syncOperationTimeout, syncRequestTimeout)
	}
}

// TestOperationDeadlineSurvivesTheResponseBody: the deadline spans the body read
// too, so cancelling it when do() returns would break every successful caller.
func TestOperationDeadlineSurvivesTheResponseBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	h := &HTTPServer{BaseURL: srv.URL, Token: "t"}
	req, err := h.newRequest(context.Background(), http.MethodGet, "/api/v1/x", nil)
	if err != nil {
		t.Fatalf("newRequest: %v", err)
	}
	resp, err := h.do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	body := make([]byte, 4)
	if _, err := resp.Body.Read(body); err != nil {
		t.Fatalf("reading the body after do() returned failed (%v): the operation context was cancelled too early", err)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// errAddrNotAvail reproduces the EADDRNOTAVAIL message isTransientLocalDialError
// matches on, without depending on a platform errno.
type errAddrNotAvail struct{}

func (errAddrNotAvail) Error() string { return "connect: cannot assign requested address" }
func (errAddrNotAvail) Timeout() bool { return false }
