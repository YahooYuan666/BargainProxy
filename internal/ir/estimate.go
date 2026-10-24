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
