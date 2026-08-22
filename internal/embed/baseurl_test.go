package embed

import "testing"

// TestNewClientHonorsBaseURLEnvVar covers a gap that was invisible until
// something needed to point at a non-default Ollama: the address was
// hardcoded at all ten NewClient call sites, so a shared GPU box, a
// container on another port, or any remote server simply could not be
// used — and since hooks and the MCP server accept no flags, there was no
// configuration route out of it either. Same shape as CLAUDE_MEM_DB.
func TestNewClientHonorsBaseURLEnvVar(t *testing.T) {
	t.Run("unset uses the local default", func(t *testing.T) {
		t.Setenv(BaseURLEnvVar, "")
		if got := NewClient("m").BaseURL; got != DefaultBaseURL {
			t.Fatalf("BaseURL = %q, want the %q default", got, DefaultBaseURL)
		}
	})

	t.Run("set is used verbatim", func(t *testing.T) {
		const remote = "http://gpu-box.internal:11434"
		t.Setenv(BaseURLEnvVar, remote)
		if got := NewClient("m").BaseURL; got != remote {
			t.Fatalf("BaseURL = %q, want %q", got, remote)
		}
	})

	t.Run("a trailing slash is trimmed", func(t *testing.T) {
		// Every call site concatenates a rooted path, so keeping the slash
		// produces "…:11434//api/embeddings" — a 404 whose cause is not
		// remotely obvious from the error text, and an easy thing to leave
		// on the end of a copy-pasted URL.
		t.Setenv(BaseURLEnvVar, "http://gpu-box.internal:11434/")
		if got := NewClient("m").BaseURL; got != "http://gpu-box.internal:11434" {
			t.Fatalf("BaseURL = %q, want the trailing slash trimmed", got)
		}
	})

	t.Run("whitespace-only falls back to the default", func(t *testing.T) {
		t.Setenv(BaseURLEnvVar, "   ")
		if got := NewClient("m").BaseURL; got != DefaultBaseURL {
			t.Fatalf("BaseURL = %q, want the default — a whitespace-only value is what an "+
				"unexpanded shell variable produces, not a real address", got)
		}
	})
}
