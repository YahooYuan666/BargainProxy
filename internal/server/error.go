package server

import (
	"bargainproxy/internal/gateway"
	"bargainproxy/internal/protocol/openai"
	"bargainproxy/internal/uperr"
)

func openAIError(err error) *openai.ErrorBody {
	_, class, msg := gateway.DescribeError(err)
	body := openai.RenderError(msg, "upstream_error", class)
	if ue, ok := uperr.As(err); ok {
		if ue.UpstreamType != "" {
			body.Error.Type = ue.UpstreamType
		}
		if ue.UpstreamCode != nil {
			body.Error.Code = ue.UpstreamCode
		}
		body.Error.Param = ue.UpstreamParam
	}
	return body
}
