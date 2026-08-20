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
	"strings"
	"time"
)

// DefaultBaseURL is Ollama's default local server address.
const DefaultBaseURL = "http://localhost:11434"

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
		BaseURL: DefaultBaseURL,
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

// Embed returns the embedding vector for text.
func (c *Client) Embed(text string) ([]float32, error) {
	body, err := json.Marshal(embedRequest{Model: c.Model, Prompt: text})
	if err != nil {
		return nil, err
	}

	resp, err := c.HTTP.Post(c.BaseURL+"/api/embeddings", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ollama embeddings request (is `ollama serve` running?): %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama embeddings returned status %d", resp.StatusCode)
	}

	var er embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		return nil, fmt.Errorf("decode ollama embeddings response: %w", err)
	}
	if len(er.Embedding) == 0 {
		return nil, fmt.Errorf("ollama returned an empty embedding — is model %q pulled? (`ollama pull %s`)", c.Model, c.Model)
	}
	return er.Embedding, nil
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
