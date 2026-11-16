package qoder

// BP-327：qoder 队列的 queueCount（plugins bf47b85 + RobbsLuo 8a96511）。
//
// 两条独立的上游教训：
//   - queueCount 缺失（null）与 0 是**不同的答案**。把 null 当 0 = 明明前面
//     排着人却立刻重发；把 0 当「照等」= 已经排到队首还睡 30s（上游实测 92 次
//     白等的 30s）。
//   - 排到队首且状态连续不动 → 该直接重发，不该把预算睡完。

import "testing"

// queueCount 的三种形态：数字、数字字符串、缺失。缺失必须留成「不知道」。
func TestBP327QueueCountParsing(t *testing.T) {
	q, ok := queueStateOf(map[string]any{"isQueued": true, "queueCount": float64(3)})
	if !ok || !q.QueueCountKnown || q.QueueCount != 3 {
		t.Fatalf("numeric queueCount: known=%v count=%d ok=%v", q.QueueCountKnown, q.QueueCount, ok)
	}
	q, ok = queueStateOf(map[string]any{"isQueued": true, "queueCount": "7"})
	if !ok || !q.QueueCountKnown || q.QueueCount != 7 {
		t.Fatalf("string queueCount: known=%v count=%d ok=%v", q.QueueCountKnown, q.QueueCount, ok)
	}
	q, ok = queueStateOf(map[string]any{"isQueued": true})
	if !ok {
		t.Fatal("state must still be recognized without queueCount")
	}
	if q.QueueCountKnown {
		t.Fatalf("absent queueCount must stay UNKNOWN (null is not 0), got known=true count=%d", q.QueueCount)
	}
	// 0 是明确答案。
	q, ok = queueStateOf(map[string]any{"isQueued": true, "queueCount": float64(0)})
	if !ok || !q.QueueCountKnown || q.QueueCount != 0 {
		t.Fatalf("zero queueCount must be KNOWN zero: known=%v count=%d", q.QueueCountKnown, q.QueueCount)
	}
	// 解不出来的字符串也不算知道。
	q, _ = queueStateOf(map[string]any{"isQueued": true, "queueCount": "many"})
	if q.QueueCountKnown {
		t.Fatalf("undecodable queueCount must stay unknown, got %d", q.QueueCount)
	}
}
