package uperr

import (
	"encoding/json"
	"strings"
)

// WithFields keeps only machine-readable error fields, never an upstream body.
func WithFields(err *Error, body []byte) *Error {
	var doc struct {
		Error struct {
			Type  string `json:"type"`
			Code  any    `json:"code"`
			Param string `json:"param"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return err
	}
	safe := func(s string) bool {
		if s == "" || len(s) > 128 {
			return false
		}
		for _, r := range s {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_.-[]", r)) {
				return false
			}
		}
		return true
	}
	if safe(doc.Error.Type) {
		err.UpstreamType = doc.Error.Type
	}
	switch v := doc.Error.Code.(type) {
	case string:
		if safe(v) {
			err.UpstreamCode = v
		}
	case float64:
		err.UpstreamCode = v
	}
	if safe(doc.Error.Param) {
		err.UpstreamParam = doc.Error.Param
	}
	return err
}
