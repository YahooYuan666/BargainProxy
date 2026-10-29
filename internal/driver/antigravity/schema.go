// Tool-schema sanitization for the Gemini functionDeclarations payload.
// The Antigravity endpoint rejects OpenAI-style JSON Schemas with 400
// "Unknown name ..." for anything beyond its narrow allowed set (observed
// live: a "schema" key inside parameters). Semantics ported from the
// AIClient2API reference converters/utils.js cleanJsonSchemaProperties
// (GPLv3 — clean-room reimplementation of the behavior, no code copied):
// allow-list type/description/properties/required/enum/items/nullable,
// upper-case the type field, translate type arrays containing "null" to
// nullable, coerce enum members to strings, and recurse into
// properties/items.
package antigravity

import (
	"strings"
)

// geminiSchemaAllowedKeys is the exact set the endpoint accepts.
var geminiSchemaAllowedKeys = map[string]bool{
	"type": true, "description": true, "properties": true,
	"required": true, "enum": true, "items": true, "nullable": true,
}

// cleanGeminiSchema recursively reduces an arbitrary JSON-schema-ish value to
// the Gemini-accepted shape. Bare booleans are normalized too (BP-192, magpie
// 2cac98d: MCP tools emit draft-2020-12 boolean schemas and the Google
// backend 400s on them): `true` accepts anything and becomes an empty schema,
// `false` matches nothing and the key carrying it is dropped by the caller.
func cleanGeminiSchema(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return cleanGeminiSchemaMap(t)
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = cleanGeminiSchema(item)
		}
		return out
	case bool:
		if t {
			return map[string]any{}
		}
		return nil
	default:
		return v
	}
}

func cleanGeminiSchemaMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		if !geminiSchemaAllowedKeys[k] {
			continue
		}
		switch k {
		case "type":
			// The type field may be an array like ["string","null"]; the
			// endpoint wants a single upper-case type plus a sibling
			// "nullable": true.
			tv, nullable := geminiTypeField(v)
			if tv != nil {
				out[k] = tv
			}
			if nullable {
				out["nullable"] = true
			}
		case "enum":
			if arr, ok := v.([]any); ok {
				strs := make([]any, 0, len(arr))
				for _, e := range arr {
					if s, ok := e.(string); ok {
						strs = append(strs, s)
					}
				}
				if len(strs) > 0 {
					out[k] = strs
				}
			}
		case "properties":
			if props, ok := v.(map[string]any); ok {
				cleaned := make(map[string]any, len(props))
				for pk, pv := range props {
					cv := cleanGeminiSchema(pv)
					if cv == nil {
						// A bare-false property can never match: drop it
						// instead of sending a null schema node (BP-192).
						continue
					}
					cleaned[pk] = cv
				}
				out[k] = cleaned
			}
		case "items":
			if cv := cleanGeminiSchema(v); cv != nil {
				out[k] = cv
			} // a bare-false items: drop the key (element shape unrestricted)
		default:
			out[k] = v
		}
	}
	return out
}

// geminiTypeField normalizes the "type" value: strings upper-cased
// (OBJECT/STRING/...), arrays like ["string","null"] collapse to
// ("STRING", true). Returns nil when nothing usable remains.
func geminiTypeField(v any) (any, bool) {
	switch t := v.(type) {
	case string:
		u := strings.ToUpper(strings.TrimSpace(t))
		if u == "" {
			return nil, false
		}
		return u, false
	case []any:
		nullable := false
		var first any
		for _, e := range t {
			s, ok := e.(string)
			if !ok {
				continue
			}
			if strings.EqualFold(s, "null") {
				nullable = true
				continue
			}
			if first == nil {
				first = strings.ToUpper(s)
			}
		}
		return first, nullable
	default:
		return nil, false
	}
}
