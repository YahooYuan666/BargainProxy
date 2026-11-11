package ir

import "fmt"

// EstimatePromptTokens is a coarse prompt-size estimate: chars/2 over text and
// thinking blocks, a flat per-image allowance, tool schema included. Exact
// accounting stays with the upstream usage — callers use this to size buffers
// and to gate channels with hard context ceilings long before a request is
// sent (BP-72 first-byte widening; BP-103 lobster oversize gate).
func EstimatePromptTokens(req *ChatRequest) int {
	if req == nil {
		return 0
	}
	chars := 0
	images := 0
	for _, m := range req.Messages {
		for _, p := range m.Content {
			switch p.Kind {
			case "text":
				chars += len(p.Text)
			case "image":
				// Prefill cost of an image is roughly fixed per tile; the
				// base64 size is not a token count and must not be used.
				images++
			}
		}
		for _, th := range m.Thinking {
			chars += len(th.Text)
		}
	}
	toolChars := 0
	for _, td := range req.Tools {
		toolChars += len(td.Name) + len(td.Description)
		if td.Parameters != nil {
			toolChars += len(fmt.Sprint(td.Parameters))
		}
	}
	return (chars+toolChars)/2 + images*1500
}

// EstimateRequestBytes is a cheap proxy for the WIRE size a driver will
// produce: text bytes + raw image base64 bytes + tool JSON bytes + thinking
// bytes. It is NOT the exact serialized body (JSON overhead, per-driver
// framing differ) — it exists so the gateway can skip channels whose BYTE
// ceilings the request cannot possibly fit before paying a dispatch on it
// (BP-312: a 6MB screenshot-laden history burned 25s on workbuddy and trae
// before lobster's 4MB gate answered in 10ms).
//
// The dominant term is ImageB64: base64 payload rides the wire verbatim in
// every OpenAI-style body builder, while EstimatePromptTokens deliberately
// ignores it (tokens-per-tile ≠ bytes). Both metrics answer different
// questions and are used side by side.
func EstimateRequestBytes(req *ChatRequest) int {
	if req == nil {
		return 0
	}
	text := 0
	images := 0
	thinking := 0
	for _, m := range req.Messages {
		for _, p := range m.Content {
			switch p.Kind {
			case "text":
				text += len(p.Text)
			case "image":
				images += len(p.ImageB64)
			}
		}
		for _, th := range m.Thinking {
			thinking += len(th.Text)
		}
	}
	tools := 0
	for _, td := range req.Tools {
		tools += len(td.Name) + len(td.Description)
		if td.Parameters != nil {
			tools += len(fmt.Sprint(td.Parameters))
		}
	}
	return text + images + thinking + tools
}
