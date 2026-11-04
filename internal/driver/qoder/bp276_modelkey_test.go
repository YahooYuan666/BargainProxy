package qoder

import "testing"

// BP-276 (magpie-community/plugins 1a99e2c, qoder 0.2.60, #692): the business
// record also names the model — the official qodercli 1.1.82 puts the chosen
// model's catalogue key there, and the server books the call's BILLING from
// it. Without it Qoder and Qoder CN booked every call as Auto whatever
// model_config named (the same disease #638 saw: the 0.2.54 model_config fix
// did not change the billing).
func TestBusinessModelKeyMatchesModelConfig(t *testing.T) {
	d, cap := newAggGateway(t)
	runCall(t, d, aggCall("DeepSeek-Flash", "hi"))
	doc := cap.at(0)
	biz, ok := doc["business"].(map[string]any)
	if !ok {
		t.Fatalf("no business record on the wire: %v", doc["business"])
	}
	mk, _ := biz["model_key"].(string)
	if mk == "" {
		t.Fatalf("business.model_key missing — upstream books the call as Auto without it (plugins#692)")
	}
	mc, _ := doc["model_config"].(map[string]any)
	mck, _ := mc["key"].(string)
	if mk != mck {
		t.Fatalf("business.model_key %q must equal model_config.key %q (official client shape, plugins#692)", mk, mck)
	}
}
