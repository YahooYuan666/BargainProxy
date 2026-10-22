package prompt

// Tool-schema repair for strict upstreams.
//
// Why: WorkBuddy (and the DeepSeek-family backends behind it) validate tool
// schemas strictly and answer 400 code 11133 "Invalid request parameters"
// when a property's enum has no usable type. MCP servers routinely emit
// `{"enum": ["a","b"]}` (no type) or `{"type": ["string","null"], "enum": [...]}`
// (multi-type), which every permissive client accepts and this upstream does
// not. Reference: magpie's WorkBuddy migration notes, 2026-10-21 (an enum with
// no type, or of several types, is typed; true/false where a schema goes is
// {} or {"not": {}}).

// FixEnumTypes walks a JSON-schema value and types every enum it finds:
//   - enum without type  -> type inferred from the enum values
//   - multi-type with enum -> type narrowed to the enum's single value type
//
// It reports whether anything changed. The input map is modified in place;
// callers pass a map they own (the driver's freshly built body).
func FixEnumTypes(schema any) bool {
	switch s := schema.(type) {
	case map[string]any:
		changed := false
		// Only a schema that actually carries an enum needs typing.
		if raw, ok := s["enum"]; ok {
			if vals, ok := raw.([]any); ok && len(vals) > 0 {
				if inferred := inferEnumType(vals); inferred != "" {
					switch t := s["type"].(type) {
					case nil:
						s["type"] = inferred
						changed = true
					case []any:
						// multi-type: an enum of one value kind must present
						// exactly that kind, or the upstream rejects the
						// combination as contradictory.
						s["type"] = inferred
						changed = true
					case string:
						_ = t // already single-typed; leave it
					}
				}
			}
		}
		// `properties`, `items`, `additionalProperties`, `$defs`, `definitions`
		// and the anyOf/oneOf/allOf branches all nest schemas.
		for k, v := range s {
			switch k {
			case "properties", "$defs", "definitions", "patternProperties":
				if m, ok := v.(map[string]any); ok {
					for _, sub := range m {
						if FixEnumTypes(sub) {
							changed = true
						}
					}
				}
			case "items", "additionalProperties", "not", "contains", "propertyNames":
				if FixEnumTypes(v) {
					changed = true
				}
			case "anyOf", "oneOf", "allOf", "prefixItems":
				if arr, ok := v.([]any); ok {
					for _, sub := range arr {
						if FixEnumTypes(sub) {
							changed = true
						}
					}
				}
			}
		}
		return changed
	case []any:
		changed := false
		for _, sub := range s {
			if FixEnumTypes(sub) {
				changed = true
			}
		}
		return changed
	}
	return false
}

// inferEnumType returns the JSON type shared by every value in the enum, or ""
// when the values are mixed (mixed enums cannot be typed without lying, so the
// caller leaves them alone and the upstream surfaces its own error).
func inferEnumType(vals []any) string {
	kind := ""
	for _, v := range vals {
		var k string
		switch v.(type) {
		case string:
			k = "string"
		case float64, int, int64:
			k = "number"
		case bool:
			k = "boolean"
		case nil:
			continue // null is legal beside any type; it does not decide it
		default:
			return "" // arrays/objects in an enum are unusual; do not guess
		}
		if kind == "" {
			kind = k
			continue
		}
		if kind != k {
			return ""
		}
	}
	return kind
}
