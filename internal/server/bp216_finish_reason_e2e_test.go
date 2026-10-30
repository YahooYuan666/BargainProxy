package server_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// BP-216: a tool round must reach an OpenAI client with finish_reason
// "tool_calls" — not the IR/Anthropic spelling "tool_use". OpenMausBot rejects
// any other value with "provider tool calls were truncated or malformed", even
// when every argument arrived intact. This test goes through the real entry
// point so a regression cannot hide behind the FinishReason helper.
func TestBP216ToolRoundFinishReasonOnOpenAIEntry(t *testing.T) {
	up := newFakeUpstream(t)
	up.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"functionCall\":{\"name\":\"get_weather\",\"args\":{\"city\":\"Paris\"}}}]},\"finishReason\":\"STOP\"}]}}\n\n")
	})
	srv, _ := buildTestServer(t, up, 1)

	toolBody := `{"model":"gemini-3-pro","messages":[{"role":"user","content":"Weather?"}],"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]}`

	t.Run("non-streaming", func(t *testing.T) {
		resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(toolBody))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out struct {
			Choices []struct {
				FinishReason string `json:"finish_reason"`
				Message      struct {
					ToolCalls []struct {
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(out.Choices) != 1 {
			t.Fatalf("choices = %+v", out.Choices)
		}
		c := out.Choices[0]
		if len(c.Message.ToolCalls) == 0 {
			t.Fatalf("no tool_calls in response: %+v", c)
		}
		if c.FinishReason != "tool_calls" {
			t.Fatalf("finish_reason = %q, want tool_calls", c.FinishReason)
		}
	})

	t.Run("streaming", func(t *testing.T) {
		body := `{"model":"gemini-3-pro","stream":true,"messages":[{"role":"user","content":"Weather?"}],"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]}`
		resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		var finish string
		var args string
		var sawDone bool
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			payload := strings.TrimPrefix(line, "data: ")
			if payload == "[DONE]" {
				sawDone = true
				continue
			}
			var chunk struct {
				Choices []struct {
					Delta struct {
						ToolCalls []struct {
							Function struct {
								Arguments string `json:"arguments"`
							} `json:"function"`
						} `json:"tool_calls"`
					} `json:"delta"`
					FinishReason *string `json:"finish_reason"`
				} `json:"choices"`
			}
			if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
				t.Fatalf("bad chunk %q: %v", payload, err)
			}
			for _, c := range chunk.Choices {
				for _, tc := range c.Delta.ToolCalls {
					args += tc.Function.Arguments
				}
				if c.FinishReason != nil {
					finish = *c.FinishReason
				}
			}
		}
		if !sawDone {
			t.Fatal("stream ended without [DONE]")
		}
		if !strings.Contains(args, "Paris") {
			t.Fatalf("tool arguments were lost: %q", args)
		}
		if finish != "tool_calls" {
			t.Fatalf("finish_reason = %q, want tool_calls", finish)
		}
	})
}
