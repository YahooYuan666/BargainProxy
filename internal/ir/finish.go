package ir

import "strings"

// FinishReasonFailed reports an OpenAI-chat finish_reason that says the TURN
// failed rather than finished. WorkBuddy and DeepSeek harnesses end a 200
// stream with finish_reason "error" when the model's turn dies mid-flight —
// every other word is a real end. Mapping it through the usual finish table
// handed the client a half answer as a success and kept the gateway from
// cascading (BP-222; magpie 86fef947 #2471; research/2026-10-31-上游参考项目更新巡检.md §1.1).
func FinishReasonFailed(reason string) bool {
	return strings.EqualFold(reason, "error")
}
