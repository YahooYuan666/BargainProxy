// ticket 30: business-envelope `msg` parsing + idempotent checkin detection.
package workbuddy

import (
	"errors"
	"testing"

	"bargainproxy/internal/uperr"
)

// TestDetectBusinessErrorReadsMsg 钉住上游 billing 信封字段名：文本在 `msg`
// （agentpoints apiEnvelope 口径），`message` 只做兜底——此前只读 message，
// 导致 code=10001 的重复签到原文丢失（空消息），无法识别幂等。
func TestDetectBusinessErrorReadsMsg(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		code int
		msg  string
		ok   bool
	}{
		{"billing 信封 msg", `{"code":10001,"msg":"今天已签到"}`, 10001, "今天已签到", true},
		{"msg 优先于 message", `{"code":1,"msg":"来自msg","message":"来自message"}`, 1, "来自msg", true},
		{"message 兜底（OpenAI 形态）", `{"code":11101,"message":"Unmarshal chat params failed"}`, 11101, "Unmarshal chat params failed", true},
		{"嵌套 error.message", `{"error":{"code":6004,"message":"模型限流"}}`, 6004, "模型限流", true},
		{"嵌套 error.msg", `{"error":{"code":429,"msg":"rate limited"}}`, 429, "rate limited", true},
		{"code=0 非错误", `{"code":0,"msg":"ok"}`, 0, "", false},
		{"非 JSON", `not json`, 0, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, msg, ok := detectBusinessError([]byte(c.raw))
			if code != c.code || msg != c.msg || ok != c.ok {
				t.Fatalf("got (%d, %q, %v), want (%d, %q, %v)", code, msg, ok, c.code, c.msg, c.ok)
			}
		})
	}
}

// TestIsAlreadyCheckin 幂等签到判定：code 10001/14001 的「已签到」/already
// 文案 → 幂等成功（不再记 FAILED）；其余错误不误判。
func TestIsAlreadyCheckin(t *testing.T) {
	if IsAlreadyCheckin(nil) {
		t.Fatal("nil 不应判为已签到")
	}
	if IsAlreadyCheckin(errors.New("boom")) {
		t.Fatal("普通错误不应判为已签到")
	}
	yes := []*uperr.Error{
		uperr.New(uperr.ClassInternal, 200, "workbuddy: code=10001 %s", "今天已签到"),
		uperr.New(uperr.ClassInternal, 200, "workbuddy: code=14001 %s", "今日已签到"),
		uperr.New(uperr.ClassBadRequest, 400, "workbuddy: code=14001 already checked in"),
	}
	for _, e := range yes {
		if !IsAlreadyCheckin(e) {
			t.Fatalf("%q 应判为已签到", e.Message)
		}
	}
	no := []*uperr.Error{
		uperr.New(uperr.ClassInternal, 200, "workbuddy: code=10001 "),
		uperr.New(uperr.ClassHardCredit, 402, "workbuddy: 积分不足"),
		uperr.New(uperr.ClassAuth, 401, "workbuddy: session dead"),
	}
	for _, e := range no {
		if IsAlreadyCheckin(e) {
			t.Fatalf("%q 不应判为已签到", e.Message)
		}
	}
}
