package panel

import (
	"strings"
	"testing"
)

// BP-71: upstream 401s from workbuddy / openresty / APISIX return entire HTML
// error pages; the panel overview must never display raw HTML in table cells.
func TestCleanProbeErrorStripsAPISIXHTML(t *testing.T) {
	raw := "upstream error class=auth status=401: workbuddy: <html>\r\n<head><title>401 Authorization Required</title></head>\r\n<body>\r\n<center><h1>401 Authorization Required</h1></center>\r\n<hr><center>openresty</center>\r\n<p><em>Powered by <a href=\"https://apisix.apache.org/\">APISIX</a>.</em></p></body>\r\n</html>\r\n"
	cleaned := cleanProbeError(raw)
	if strings.Contains(cleaned, "<") || strings.Contains(cleaned, ">") {
		t.Fatalf("cleaned error must not contain HTML tags: %q", cleaned)
	}
	if !strings.Contains(cleaned, "401") || !strings.Contains(cleaned, "凭据失效") {
		t.Fatalf("expected 401 human-readable summary, got %q", cleaned)
	}
}

func TestCleanProbeErrorMappings(t *testing.T) {
	cases := map[string]string{
		"403 Forbidden":             "403 权限不足或被风控拦截",
		"rate limit exceeded (429)": "429 频率受限 / 额度耗尽",
		"502 Bad Gateway":           "502 上游网关错误",
		"503 Service Unavailable":   "503 上游服务暂不可用",
		"context deadline exceeded": "探测超时（上游未在时限内响应）",
		"plain short error":         "plain short error",
		"":                          "探测失败",
	}
	for in, want := range cases {
		if got := cleanProbeError(in); got != want {
			t.Errorf("cleanProbeError(%q) = %q, want %q", in, got, want)
		}
	}
}
