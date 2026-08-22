// ContentHash lives here (not in either backend) because the dedup key is
// a property of the event, not of the database that stores it.
package memory

import (
	"crypto/sha256"
	"encoding/hex"
)

// ContentHash returns the dedup key for one tool call within one session.
func ContentHash(sessionID, toolName, toolInput, toolOutput string) string {
	h := sha256.New()
	h.Write([]byte(sessionID))
	h.Write([]byte{0})
	h.Write([]byte(toolName))
	h.Write([]byte{0})
	h.Write([]byte(toolInput))
	h.Write([]byte{0})
	h.Write([]byte(toolOutput))
	return hex.EncodeToString(h.Sum(nil))
}
