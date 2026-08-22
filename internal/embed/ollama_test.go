package embed

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPingModelPulled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"models":[{"name":"nomic-embed-text:latest"},{"name":"llama3.1:8b"}]}`))
	}))
	defer srv.Close()

	c := NewClient("nomic-embed-text")
	c.BaseURL = srv.URL
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping() with the model present in /api/tags: want nil, got %v", err)
	}
}

func TestPingModelNotPulled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"models":[{"name":"llama3.1:8b"}]}`))
	}))
	defer srv.Close()

	c := NewClient("nomic-embed-text")
	c.BaseURL = srv.URL
	err := c.Ping(context.Background())
	if err == nil {
		t.Fatal("Ping() with the model absent from /api/tags: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "not pulled") {
		t.Errorf("error = %q, want it to say the model isn't pulled", err.Error())
	}
}

func TestPingServerUnreachable(t *testing.T) {
	c := NewClient("nomic-embed-text")
	c.BaseURL = "http://127.0.0.1:1" // nothing listens here
	err := c.Ping(context.Background())
	if err == nil {
		t.Fatal("Ping() against an unreachable server: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "not reachable") {
		t.Errorf("error = %q, want it to say Ollama isn't reachable (distinct from 'model not pulled')", err.Error())
	}
}

func TestPingServerErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient("nomic-embed-text")
	c.BaseURL = srv.URL
	if err := c.Ping(context.Background()); err == nil {
		t.Fatal("Ping() against a server returning 500: want an error, got nil")
	}
}

// TestEmbedRetriesOnceOnServerError is the regression test for the real
// gap: a single transient failure (here, a 500 on the first call) used to
// fail the embedding permanently. It must now succeed via the retry.
func TestEmbedRetriesOnceOnServerError(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Write([]byte(`{"embedding":[1,2,3]}`))
	}))
	defer srv.Close()

	c := NewClient("nomic-embed-text")
	c.BaseURL = srv.URL
	vec, err := c.Embed(context.Background(), "some text")
	if err != nil {
		t.Fatalf("Embed after one transient 500: want it to succeed on retry, got error: %v", err)
	}
	if len(vec) != 3 {
		t.Fatalf("Embed returned %v, want the second call's vector [1 2 3]", vec)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("server received %d calls, want exactly 2 (the failure, then one retry)", got)
	}
}

// flakyTransport fails the first RoundTrip with a transport-level error
// (simulating a dropped connection/DNS blip — a real network failure, not
// an HTTP status) and passes every subsequent call through to inner.
type flakyTransport struct {
	calls atomic.Int32
	inner http.RoundTripper
}

func (t *flakyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.calls.Add(1) == 1 {
		return nil, fmt.Errorf("simulated network error")
	}
	return t.inner.RoundTrip(req)
}

// TestEmbedRetriesOnceOnNetworkError covers the other transient case: the
// request never reaching the server at all (a transport-level error), not
// just a 5xx response from a server that did respond.
func TestEmbedRetriesOnceOnNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"embedding":[4,5,6]}`))
	}))
	defer srv.Close()

	transport := &flakyTransport{inner: http.DefaultTransport}
	c := NewClient("nomic-embed-text")
	c.BaseURL = srv.URL
	c.HTTP = &http.Client{Transport: transport}

	vec, err := c.Embed(context.Background(), "some text")
	if err != nil {
		t.Fatalf("Embed after one simulated network error: want it to succeed on retry, got error: %v", err)
	}
	if len(vec) != 3 {
		t.Fatalf("Embed returned %v, want a 3-element vector", vec)
	}
	if got := transport.calls.Load(); got != 2 {
		t.Fatalf("transport received %d calls, want exactly 2 (the failure, then one retry)", got)
	}
}

// TestEmbedDoesNotRetryOnEmptyEmbedding confirms a non-transient failure
// (the model genuinely isn't pulled — a successful response with no
// embedding) is NOT retried: retrying identically won't fix it, and
// retrying would only double the wasted request.
func TestEmbedDoesNotRetryOnEmptyEmbedding(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(`{"embedding":[]}`))
	}))
	defer srv.Close()

	c := NewClient("nomic-embed-text")
	c.BaseURL = srv.URL
	if _, err := c.Embed(context.Background(), "some text"); err == nil {
		t.Fatal("Embed with an empty embedding response: want an error, got nil")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("server received %d calls, want exactly 1 (no retry on a non-transient failure)", got)
	}
}

// TestEmbedDoesNotRetryOnClientErrorStatus confirms a 4xx (something about
// THIS request is wrong) also isn't retried.
func TestEmbedDoesNotRetryOnClientErrorStatus(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	c := NewClient("nomic-embed-text")
	c.BaseURL = srv.URL
	if _, err := c.Embed(context.Background(), "some text"); err == nil {
		t.Fatal("Embed with a 400 response: want an error, got nil")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("server received %d calls, want exactly 1 (no retry on a 4xx)", got)
	}
}

// TestEmbedGivesUpAfterMaxAttempts confirms a persistently failing server
// (not just one transient blip) still eventually returns an error rather
// than retrying forever.
func TestEmbedGivesUpAfterMaxAttempts(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient("nomic-embed-text")
	c.BaseURL = srv.URL
	if _, err := c.Embed(context.Background(), "some text"); err == nil {
		t.Fatal("Embed against a persistently-failing server: want an error, got nil")
	}
	if got := calls.Load(); got != embedMaxAttempts {
		t.Fatalf("server received %d calls, want exactly %d (bounded retries, not unbounded)", got, embedMaxAttempts)
	}
}

// TestEmbedCapsResponseSize: a reply larger than maxResponseBytes must be
// rejected as a decode error rather than read into memory whole.
func TestEmbedCapsResponseSize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"embedding":[`))
		chunk := []byte(strings.Repeat("1,", 1<<16))
		for written := 0; written < maxResponseBytes+len(chunk); written += len(chunk) {
			w.Write(chunk)
		}
		w.Write([]byte(`1]}`))
	}))
	defer srv.Close()

	c := NewClient("nomic-embed-text")
	c.BaseURL = srv.URL
	if _, err := c.Embed(context.Background(), "some text"); err == nil {
		t.Fatal("Embed with an oversized response: want a decode error, got nil")
	}
}

// TestEmbedHonorsContextCancel: a cancelled ctx must abort the request
// (and not count as a transient failure worth retrying).
func TestEmbedHonorsContextCancel(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		<-release
	}))
	defer srv.Close()
	defer close(release)

	c := NewClient("nomic-embed-text")
	c.BaseURL = srv.URL
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := c.Embed(ctx, "some text")
	if err == nil {
		t.Fatal("Embed with an expired ctx: want an error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("server received %d calls, want 1 (no retry after ctx cancellation)", got)
	}
}

func TestPingHonorsContextCancel(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)

	c := NewClient("nomic-embed-text")
	c.BaseURL = srv.URL
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := c.Ping(ctx); err == nil {
		t.Fatal("Ping with an expired ctx: want an error, got nil")
	}
}
