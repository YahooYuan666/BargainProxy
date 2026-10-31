package gateway

import "regexp"

// BP-233 (magpie cae785a3): a Chat Completions 400 that names the token
// budget field it WANTS is the vendor telling us the fix in its own words —
// OpenAI's new models say "Use 'max_completion_tokens' instead" while
// domestic endpoints say the reverse. One same-account retry with the field
// swapped answers it; nothing has reached the client (a 400 means the turn
// never started).
var tokenFieldAskedRe = regexp.MustCompile("(?i)\\buse\\s+['\"`]?(max_completion_tokens|max_tokens)['\"`]?")

// tokenFieldWanted returns the field the refusal asks for in place of the
// one sent, or "".
func tokenFieldWanted(msg string) string {
	m := tokenFieldAskedRe.FindStringSubmatch(msg)
	if m == nil {
		return ""
	}
	return m[1]
}
