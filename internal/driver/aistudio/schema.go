// Schema sanitizer for the functionDeclarations payload (BP-326).
//
// The generateContent schema validates against Google's own OpenAPI subset;
// rich JSON Schema keys ($schema, anyOf/oneOf, additionalProperties, const…)
// earn a 400 "Unknown name" — the same failure family BP-321 fixed on the
// CloudCode plane, where the full rewrite lives. This pass keeps the
// vocabulary the API documents and drops the rest, so a tool degrades to its
// documented shape instead of poisoning the whole request. Nodes that lose
// their type get it re-inferred from what survives (properties→object,
// enum→string, items→array); a node with nothing expressible left is dropped
// entirely — a smaller tool beats a 400.
package aistudio

var schemaKeys = map[string]bool{
	"type": true, "format": true, "description": true, "nullable": true,
	"enum": true, "items": true, "properties": true, "required": true,
	"minimum": true, "maximum": true, "minLength": true, "maxLength": true,
	"minItems": true, "maxItems": true,
}

// sanitizeSchema returns the requestable subset of a raw JSON-Schema tree.
func sanitizeSchema(v any) map[string]any {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, val := range m {
		if !schemaKeys[k] {
			continue
		}
		switch k {
		case "properties":
			props, ok := val.(map[string]any)
			if !ok {
				continue
			}
			next := make(map[string]any, len(props))
			for name, pv := range props {
				if s := sanitizeSchema(pv); s != nil {
					next[name] = s
				}
			}
			out[k] = next
		case "items":
			if s := sanitizeSchema(val); s != nil {
				out[k] = s
			}
		default:
			out[k] = val
		}
	}
	if _, ok := out["type"]; !ok {
		switch {
		case hasKey(out, "properties"):
			out["type"] = "object"
		case hasKey(out, "enum"):
			out["type"] = "string"
		case hasKey(out, "items"):
			out["type"] = "array"
		}
	}
	if !hasKey(out, "type") {
		return nil // nothing the API can express — drop the node
	}
	return out
}

func hasKey(m map[string]any, k string) bool {
	_, ok := m[k]
	return ok
}
