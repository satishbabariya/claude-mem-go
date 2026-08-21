// Package privacy strips claude-mem's fixed set of privacy tags —
// <private>, <claude-mem-context>, <system_instruction>,
// <system-instruction>, <persisted-output>, <system-reminder> — from text
// before it's sent to an observer LLM, embedded, or persisted. Mirrors real
// claude-mem's src/utils/tag-stripping.ts exactly in tag set and behavior
// (documented for a user directly in its UserPromptSubmit banner: wrap
// anything in <private>...</private> to keep it out of memory).
package privacy

import (
	"regexp"
	"strings"
)

// tagNames is TAG_NAMES from tag-stripping.ts, verbatim.
var tagNames = []string{
	"private",
	"claude-mem-context",
	"system_instruction",
	"system-instruction",
	"persisted-output",
	"system-reminder",
}

// MaxTagCount mirrors tag-stripping.ts's MAX_TAG_COUNT. Not a hard limit —
// every match is still stripped regardless — just a threshold a caller may
// want to log past, the same way tag-stripping.ts logs a warning rather
// than rejecting anything.
const MaxTagCount = 100

// tagRegexes holds one compiled pattern per tag name. Go's regexp package
// (RE2) has no backreference support, so this can't be tag-stripping.ts's
// single combined regex with a `\1` backreference tying each open tag to
// its matching close tag by name — one literal pattern per tag name is the
// RE2-compatible equivalent: each only ever matches its own open/close
// pair, which is exactly what the backreference restricts the TS version
// to as well. For same-tag nesting (the case tag-stripping.ts's own tests
// exercise) the two approaches produce identical results, matching the
// nearest same-name closing tag non-greedily either way. They can differ
// only in which tag's counter gets credited when two DIFFERENT tag types
// are nested inside each other — an edge case this package doesn't expose
// a per-tag breakdown for, only a total, so it isn't observable.
var tagRegexes = buildTagRegexes()

func buildTagRegexes() []*regexp.Regexp {
	res := make([]*regexp.Regexp, len(tagNames))
	for i, name := range tagNames {
		q := regexp.QuoteMeta(name)
		res[i] = regexp.MustCompile(`(?s)<` + q + `\b[^>]*>.*?</` + q + `>`)
	}
	return res
}

// StripTags removes every privacy-tag block from input, returning the
// cleaned, whitespace-trimmed text and the total number of tags removed
// across all tag names combined.
func StripTags(input string) (string, int) {
	stripped := input
	total := 0
	for _, re := range tagRegexes {
		stripped = re.ReplaceAllStringFunc(stripped, func(string) string {
			total++
			return ""
		})
	}
	return strings.TrimSpace(stripped), total
}

// StripMemoryTags is StripTags without the tag count, for callers that
// don't need it — the direct equivalent of tag-stripping.ts's own
// stripMemoryTags convenience wrapper around stripTags.
func StripMemoryTags(content string) string {
	stripped, _ := StripTags(content)
	return stripped
}

// maxProtocolPayloadBytes mirrors tag-stripping.ts's MAX_PROTOCOL_PAYLOAD_BYTES.
const maxProtocolPayloadBytes = 256 * 1024

const (
	protocolTagName    = "task-notification"
	protocolOpenPrefix = "<" + protocolTagName
	protocolCloseTag   = "</" + protocolTagName + ">"
)

func isWordByte(b byte) bool {
	return b == '_' || ('a' <= b && b <= 'z') || ('A' <= b && b <= 'Z') || ('0' <= b && b <= '9')
}

// IsInternalProtocolPayload reports whether text is, in its entirety
// (aside from surrounding whitespace), a single
// <task-notification>...</task-notification> block — a synthetic Claude
// Code protocol message (a background/subagent task-completion
// notification) that got auto-submitted as a UserPromptSubmit prompt, not
// real user text. Ports tag-stripping.ts's isInternalProtocolPayload.
//
// TS's version is one regex combining a `\1` backreference AND a negative
// lookahead keyed off that same backreference
// (`(?:(?!<\1\b|</\1\b)[\s\S])*`) — Go's RE2 engine can express neither.
// Implemented instead with plain string operations verifying the whole
// (trimmed) string is exactly one open tag, a body containing no further
// occurrence of the tag name, and the matching close tag — the same
// shape the backreference restricts the TS version to. The one place
// this can diverge from TS: TS's `\b` word boundary would reject a
// differently-named tag whose name happens to start with
// "task-notification" followed immediately by another word character
// (e.g. a hypothetical "<task-notificationX>"), which this port also
// rejects via the isWordByte check below — matched deliberately, not
// coincidentally.
func IsInternalProtocolPayload(text string) bool {
	if text == "" {
		return false
	}
	if len(text) > maxProtocolPayloadBytes {
		return false
	}
	s := strings.TrimSpace(text)
	if !strings.HasPrefix(s, protocolOpenPrefix) {
		return false
	}
	rest := s[len(protocolOpenPrefix):]
	if rest == "" || isWordByte(rest[0]) {
		return false
	}
	gt := strings.IndexByte(rest, '>')
	if gt < 0 {
		return false
	}
	body := rest[gt+1:]
	if !strings.HasSuffix(body, protocolCloseTag) {
		return false
	}
	inner := body[:len(body)-len(protocolCloseTag)]
	if strings.Contains(inner, protocolOpenPrefix) || strings.Contains(inner, "</"+protocolTagName) {
		// A second occurrence of the tag name anywhere in the body — two
		// adjacent/separated blocks, or a stray nested one — means this
		// isn't a single well-formed block spanning the whole string.
		return false
	}
	return true
}
