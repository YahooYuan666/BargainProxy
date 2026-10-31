package uperr

import "regexp"

// BP-232 (magpie cfcd6648, #4420): the two Chinese word families every
// domestic vendor answers with — a prompt TOO LONG versus a HOW-FAST limit.
// The distinction decides the account's fate: an overflow is the request's
// fault (no rotation can shorten it), while a pace cap is the account's
// moment (another account passes). The killer shape is one sentence carrying
// both — 「内容太长，已达到速率限制」 is the momentary cap, so the pace words
// are checked FIRST wherever this matters.

// paceWordsRe say a limit on how fast or how much, in Chinese too.
var paceWordsRe = regexp.MustCompile(`(?i)rate.?limit|per (minute|hour|day)|\bTP[MD]\b|please wait|try again later|throttl|速率|频繁|频率|限流|并发|每(秒|分钟|小时|天|日)|稍后(再试|重试)`)

// overflowRe says the prompt itself did not fit. English forms plus the
// Chinese families: 「(内容|输入|提示词…)太长/过长/超长」, 「…超出…长度/token」.
var overflowRe = regexp.MustCompile(`(?i)context_length_exceeded|prompt is too long|input is too long|(exceeds?|exceeded|over|beyond)( the)?( model'?s?)?( maximum)? context|context (length|limit|window) (exceeded|is exceeded)|maximum context length|too many (input |prompt )?tokens|上下文(长度)?(超|过长)|超(过|出)(了)?(模型)?(的)?(最大)?上下文` +
	`|(内容|输入|文本|提示词?|prompt|问题|对话|消息)(内容|文本)?(长度)?\s*(太长|过长|超长)|字数(太多|过多)` +
	`|(内容|输入|文本|提示词?|prompt|问题|对话|消息)[^，。,.;；!！]{0,12}超(过|出)[^，。,.;；!！]{0,12}(长度|token|令牌|字数)` +
	`|(输入|提示词?|prompt)\s*的?\s*(长度|token|令牌|字数)[^，。,.;；!！]{0,8}超(过|出)`)

// PaceLimited reports text that words a how-fast limit (Chinese included).
func PaceLimited(text string) bool {
	return paceWordsRe.MatchString(text)
}

// Overflowed reports text that words a prompt-too-long refusal.
func Overflowed(text string) bool {
	return overflowRe.MatchString(text)
}

// MarkOverflow sets ContextOverflow on err when the text words a prompt-too-long
// refusal (BP-244). It returns the same error so callers can wrap it inline:
//
//	return uperr.MarkOverflow(e, bodyText)
//
// The flag is what lets the protocol writers tell the client "compact and
// retry" instead of handing back a generic 400 that a client like Codex will
// answer by resending the identical oversized body.
func MarkOverflow(err *Error, text string) *Error {
	// Pace words decide FIRST, exactly as BP-232 established for classification:
	// 「内容太长，已达到速率限制」 carries overflow wording but is a momentary
	// rate cap — the same request can pass a minute later on another account.
	// Labelling it an overflow would tell the client to permanently shrink a
	// prompt that was never too big.
	if err != nil && !PaceLimited(text) && Overflowed(text) {
		err.ContextOverflow = true
	}
	return err
}
