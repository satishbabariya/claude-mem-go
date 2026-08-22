// Package embed generates text embeddings via a local Ollama server —
// deliberately not a paid API (Voyage/OpenAI/Gemini). Consistent with the
// rest of this exploration's stance on CMEM Pro's paid dependencies: local
// inference at $0/mo instead of billing per embedding, using the same
// self-hosted-model pattern already validated for observation generation
// itself (an Ollama/LM-Studio-compatible endpoint via
// CLAUDE_MEM_OPENROUTER_BASE_URL in the real claude-mem).
//
// This is real claude-mem's semantic-search analog (it syncs observations
// into Chroma for embedding-based search) reimplemented on a local model
// instead of a hosted vector service.
package embed

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// DefaultBaseURL is Ollama's default local server address.
const DefaultBaseURL = "http://localhost:11434"

// BaseURLEnvVar points every Ollama call at a different server.
//
// The address was hardcoded at all ten NewClient call sites, so a setup
// where Ollama runs anywhere but this machine's own :11434 — a shared GPU
// box, a container on a different port, a colleague's workstation — could
// not embed at all, and the failure had no configuration route out of it.
// This is the same shape of gap as CLAUDE_MEM_DB (see store.DBPathEnvVar):
// hooks and the MCP server take no flags, so an env var is the only way
// they can be told anything.
//
// Named after the CLAUDE_MEM_* family already used elsewhere here, and
// directly analogous to real claude-mem's own CLAUDE_MEM_OPENROUTER_BASE_URL,
// which exists so its model calls can be pointed at an Ollama/LM-Studio
// endpoint for exactly this reason.
const BaseURLEnvVar = "CLAUDE_MEM_OLLAMA_BASE_URL"

// resolveBaseURL is $CLAUDE_MEM_OLLAMA_BASE_URL when set, else
// DefaultBaseURL. A trailing slash is trimmed because every call site
// concatenates a rooted path ("/api/embeddings"), and "…:11434//api/…"
// is a 404 whose cause is not obvious from the error.
func resolveBaseURL() string {
	v := strings.TrimSpace(os.Getenv(BaseURLEnvVar))
	if v == "" {
		return DefaultBaseURL
	}
	return strings.TrimRight(v, "/")
}

// Client calls Ollama's /api/embeddings endpoint.
type Client struct {
	BaseURL string
	Model   string
	HTTP    *http.Client
}

// NewClient returns a Client for model (e.g. "nomic-embed-text") against
// Ollama's default local address.
func NewClient(model string) *Client {
	return &Client{
		BaseURL: resolveBaseURL(),
		Model:   model,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

type embedRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

type embedResponse struct {
	Embedding []float32 `json:"embedding"`
}

// embedMaxAttempts bounds how many times Embed retries a transient
// failure — matching the "exactly one retry" pattern classify/worker.go
// already established for the main observer calls, not open-ended
// backoff. Before this, a single network hiccup against Ollama (briefly
// unavailable, momentarily overloaded) failed the embedding permanently
// for that observation — worker.process treats an embedding failure as
// non-fatal ("additive only," logs and moves on), so this wasn't
// catastrophic, but it meant semantic search silently and permanently
// missed observations on any brief Ollama blip a retry would have
// recovered from.
const embedMaxAttempts = 2

// Embed returns the embedding vector for text, retrying once on a
// transient failure (a network-level error reaching Ollama at all, or a
// 5xx status) but not on one that retrying identically won't fix (a 4xx
// status, or a successful response with an empty embedding — the model
// genuinely isn't pulled, and it won't be moments later either).
func (c *Client) Embed(text string) ([]float32, error) {
	var lastErr error
	for attempt := 1; attempt <= embedMaxAttempts; attempt++ {
		vec, retryable, err := c.embedOnce(text)
		if err == nil {
			return vec, nil
		}
		lastErr = err
		if !retryable {
			break
		}
	}
	return nil, lastErr
}

func (c *Client) embedOnce(text string) (vec []float32, retryable bool, err error) {
	body, err := json.Marshal(embedRequest{Model: c.Model, Prompt: text})
	if err != nil {
		return nil, false, err
	}

	resp, err := c.HTTP.Post(c.BaseURL+"/api/embeddings", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, true, fmt.Errorf("ollama embeddings request (is `ollama serve` running?): %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 {
		return nil, true, fmt.Errorf("ollama embeddings returned status %d", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("ollama embeddings returned status %d", resp.StatusCode)
	}

	var er embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		return nil, false, fmt.Errorf("decode ollama embeddings response: %w", err)
	}
	if len(er.Embedding) == 0 {
		return nil, false, fmt.Errorf("ollama returned an empty embedding — is model %q pulled? (`ollama pull %s`)", c.Model, c.Model)
	}
	return er.Embedding, false, nil
}

type tagsResponse struct {
	Models []struct {
		Name string `json:"name"`
	} `json:"models"`
}

// Ping checks that Ollama is reachable and that c.Model is actually pulled —
// the read path for `claude-mem-go doctor`. Cheaper than a real Embed() call
// (no inference cost) and more diagnostic: it distinguishes "server not
// running" from "server running but this model was never pulled," which
// Embed's own error message can only guess at from an empty-embedding
// response.
func (c *Client) Ping() error {
	resp, err := c.HTTP.Get(c.BaseURL + "/api/tags")
	if err != nil {
		return fmt.Errorf("ollama not reachable at %s (is `ollama serve` running?): %w", c.BaseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama at %s returned status %d", c.BaseURL, resp.StatusCode)
	}

	var tr tagsResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return fmt.Errorf("decode ollama /api/tags response: %w", err)
	}
	for _, m := range tr.Models {
		if m.Name == c.Model || strings.HasPrefix(m.Name, c.Model+":") {
			return nil
		}
	}
	return fmt.Errorf("ollama is running but model %q is not pulled (`ollama pull %s`)", c.Model, c.Model)
}

// ObservationText builds the text an observation's embedding is computed
// from: title, subtitle, narrative, and facts joined into one blob. Takes
// plain fields rather than a store.Observation so this package doesn't need
// to import store — the caller already has these fields either way.
func ObservationText(title, subtitle, narrative string, facts []string) string {
	parts := []string{title, subtitle, narrative}
	parts = append(parts, facts...)
	nonEmpty := parts[:0]
	for _, p := range parts {
		if p != "" {
			nonEmpty = append(nonEmpty, p)
		}
	}
	return strings.Join(nonEmpty, ". ")
}
