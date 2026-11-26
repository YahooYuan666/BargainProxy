// BP-354 (magpie-community/plugins de308730 + 1e4844b3): two identity lines in
// a SYSTEM prompt make Qoder refuse the request — neither is about the model,
// the payload size or the tools, so nothing else in the request can fix it.
//
// # 1. Claude Code's identity line → 400 "Cannot find model"
//
// Qoder answers a system prompt holding Claude Code's first line,
// "You are Claude Code, Anthropic's official CLI for Claude.", with 400
// "Cannot find model" — on ANY model (ByFanX on X, tweet 2125095548424511906;
// plugins 0.2.72). The same request with only that line swapped for the Claude
// Agent SDK's form ("You are a Claude agent, built on Anthropic's Claude Agent
// SDK.") goes through. Claude Code's other variant — the same sentence with
// ", running within the Claude Agent SDK." — is swapped the same way.
//
// # 2. DeepSeek Harness's opener → a queue that never moves
//
// A system prompt holding "You are an AI agent powered by DeepSeek Harness."
// puts the request in Qoder CN's 10605 queue with nothing ahead of it and
// keepalives until the agent gives up (plugins#1517, #1220). lirenQa1 bisected
// it on a captured body replayed three times per variant: the flip sits on
// exactly those 48 characters — one character shorter, a neutral line or no
// line at all is answered at once, while tools, size (9K to 2.7MB),
// max_completion_tokens, message count and the session header changed nothing.
// A comma before "powered by" says the same thing and passes.
//
// # Scope
//
// Only the SYSTEM text Qoder is sent is rewritten. The user's own messages,
// tool results and everything else go as written — a client's own words are
// not ours to edit, and a user quoting either sentence must reach the model
// verbatim.
package qoder

import (
	"regexp"
	"strings"
)

// claudeCodeLine matches Claude Code's identity sentence whatever its white
// space, plus the SDK variant that appends ", running within the Claude Agent
// SDK." (plugins de308730's CLAUDE_CODE_LINE).
var claudeCodeLine = regexp.MustCompile(`You\s+are\s+Claude\s+Code,\s+Anthropic's\s+official\s+CLI\s+for\s+Claude(?:,\s+running\s+within\s+the\s+Claude\s+Agent\s+SDK)?\.`)

// agentSDKLine is the sentence that passes Qoder.
const agentSDKLine = "You are a Claude agent, built on Anthropic's Claude Agent SDK."

// harnessOpener matches the "You are … agent/assistant <verb> by/on/in …"
// opener family. The match must be the line WHOLE, as WorkBuddy's own OPENERS
// is, so a comma before the verb is all that separates a queued request from
// an answered one (plugins 1e4844b3's HARNESS_OPENER).
var harnessOpener = regexp.MustCompile(`(?i)(\bYou are (?:an?|the)\s[^\n.,]{0,60}?\b(?:agent|assistant))\s+(powered by|running on|running in|built on|based on)\b`)

// asAgentSDK swaps Claude Code's identity sentence for the Agent SDK's.
// Idempotent: the replacement text does not match the pattern again.
func asAgentSDK(text string) string {
	return claudeCodeLine.ReplaceAllString(text, agentSDKLine)
}

// harnessComma puts a comma before the harness verb, keeping the name and the
// meaning. Idempotent: once the comma is there the pattern no longer matches.
func harnessComma(text string) string {
	return harnessOpener.ReplaceAllString(text, "$1, $2")
}

// sanitizeSystemText applies both rewrites to one system/developer message.
func sanitizeSystemText(text string) string {
	if text == "" {
		return text
	}
	return harnessComma(asAgentSDK(text))
}

// isSystemRole reports whether a converted message is one Qoder treats as the
// system prompt. `developer` arrives here already folded to `system`
// (convertIncomingMessage), but both are accepted so a future path cannot
// silently skip the rewrite.
func isSystemRole(role string) bool {
	return strings.EqualFold(role, "system") || strings.EqualFold(role, "developer")
}
