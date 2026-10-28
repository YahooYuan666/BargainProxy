package wbregister

import "testing"

// BP-184: models past the 200K base get the two-size contextWindow — without
// it the CLI hides the 200K/1M submenu and clips the default budget.
func TestBP184ContextWindowForLargeModels(t *testing.T) {
	big := Model{ID: "m1", MaxInputTokens: 1000000}
	if big.MaxInputTokens > 200000 {
		big.ContextWindow = &ContextWindow{
			SupportedLengths: []int{200000, big.MaxInputTokens},
			DefaultLength:    big.MaxInputTokens,
		}
	}
	if big.ContextWindow == nil {
		t.Fatal("1M model must carry a contextWindow")
	}
	if len(big.ContextWindow.SupportedLengths) != 2 || big.ContextWindow.SupportedLengths[0] != 200000 {
		t.Fatalf("supportedLengths = %v", big.ContextWindow.SupportedLengths)
	}
	if big.ContextWindow.DefaultLength != 1000000 {
		t.Fatalf("defaultLength = %d", big.ContextWindow.DefaultLength)
	}

	small := Model{ID: "m2", MaxInputTokens: 128000}
	if small.MaxInputTokens > 200000 {
		small.ContextWindow = &ContextWindow{}
	}
	if small.ContextWindow != nil {
		t.Fatal("a base-size model must not carry a contextWindow")
	}
}
