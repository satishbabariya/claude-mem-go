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
