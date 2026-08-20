package embed

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPingModelPulled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"models":[{"name":"nomic-embed-text:latest"},{"name":"llama3.1:8b"}]}`))
	}))
	defer srv.Close()

	c := NewClient("nomic-embed-text")
	c.BaseURL = srv.URL
	if err := c.Ping(); err != nil {
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
	err := c.Ping()
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
	err := c.Ping()
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
	if err := c.Ping(); err == nil {
		t.Fatal("Ping() against a server returning 500: want an error, got nil")
	}
}
