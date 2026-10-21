package ir

import "testing"

func TestHasImages(t *testing.T) {
	textOnly := []Message{{Role: RoleUser, Content: []ContentPart{{Kind: "text", Text: "hi"}}}}
	if HasImages(textOnly) {
		t.Fatal("text-only request must report no images")
	}
	withImage := []Message{{Role: RoleUser, Content: []ContentPart{
		{Kind: "text", Text: "look"},
		{Kind: "image", ImageMIME: "image/png", ImageB64: "AAAA"},
	}}}
	if !HasImages(withImage) {
		t.Fatal("image part must be detected (AutoClaw relay is text-only)")
	}
}
