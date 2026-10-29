package prompt

import (
	"encoding/json"
	"strings"
)

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
		// BP-128: a oneOf whose every branch is an object (or a $ref) needs
		// "type":"object" at this level too — Kimi answers 400 "Invalid
		// request parameters" to an untyped object union (magpie workbuddy
		// 0.1.33 / qoder 0.2.35). A schema already carrying a type is left
		// alone; anyOf has no field evidence yet and stays untouched.
		if branches, ok := s["oneOf"].([]any); ok && len(branches) > 0 && s["type"] == nil {
			allObjects := true
			for _, b := range branches {
				bm, ok := b.(map[string]any)
				if !ok {
					allObjects = false
					break
				}
				if t, _ := bm["type"].(string); t != "object" {
					if _, hasRef := bm["$ref"]; !hasRef {
						allObjects = false
						break
					}
				}
			}
			if allObjects {
				s["type"] = "object"
				changed = true
			}
		}
		// Only a schema that actually carries an enum needs typing.
		if raw, ok := s["enum"]; ok {
			vals, ok := raw.([]any)
			if !ok {
				// A hand-built Go map can carry a typed slice ([]string…);
				// round-trip it into the JSON shape the inference expects.
				if b, err := json.Marshal(raw); err == nil {
					var conv []any
					if json.Unmarshal(b, &conv) == nil {
						vals = conv
					}
				}
			}
			if len(vals) > 0 {
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

// maxRefDepth bounds nested $ref expansion so a $defs cycle (X -> Y -> X)
// stops instead of running forever; the deepest ref is left as-is.
const maxRefDepth = 32

// ExpandLocalRefs resolves local "$ref": "#/$defs/Name" pointers inline and
// strips the root "$defs"/"definitions" table. Kimi answers 400 to any tool
// schema still carrying a $ref (misakano 854e74cd, 2026-10-29), and MCP tool
// definitions share their branches through exactly such refs — so nothing is
// dropped or trimmed, every node is expanded in place.
//
// Pointers are resolved against the ROOT document's defs table (JSON Schema
// pointers are root-anchored); nested defs tables inside branches register
// into the same pool so local shorthands keep working. A pointer that cannot
// be resolved stays as-is for the upstream to complain about, same as before.
// It reports whether anything changed.
func ExpandLocalRefs(schema any) bool {
	root, ok := schema.(map[string]any)
	if !ok {
		return false
	}
	defs := map[string]any{}
	for _, key := range []string{"$defs", "definitions"} {
		if m, ok := root[key].(map[string]any); ok {
			for name, def := range m {
				if _, dup := defs[name]; !dup {
					defs[name] = def
				}
			}
			delete(root, key)
		}
	}
	if len(defs) == 0 {
		return false
	}
	return resolveRefs(root, defs, 0)
}

// resolveRefs walks a schema value replacing every resolvable local $ref
// with a deep copy of its target. Sibling keys a caller set next to $ref
// survive: the target's keys are merged over them.
func resolveRefs(v any, defs map[string]any, depth int) bool {
	if depth > maxRefDepth {
		return false
	}
	changed := false
	switch t := v.(type) {
	case map[string]any:
		if ref, ok := t["$ref"].(string); ok {
			if def, ok := lookupLocalRef(defs, ref); ok {
				for k := range t {
					delete(t, k)
				}
				if cp := deepCopyJSON(def); cp != nil {
					if m, ok := cp.(map[string]any); ok {
						for k, kv := range m {
							t[k] = kv
						}
					}
				}
				changed = true
				// The expanded body may itself carry refs (defs referencing
				// defs); one more pass at a deeper bound unwinds them.
				if resolveRefs(t, defs, depth+1) {
					changed = true
				}
				return changed
			}
		}
		for k, sub := range t {
			if k == "$ref" {
				continue
			}
			if resolveRefs(sub, defs, depth) {
				changed = true
			}
		}
	case []any:
		for _, sub := range t {
			if resolveRefs(sub, defs, depth) {
				changed = true
			}
		}
	}
	return changed
}

// lookupLocalRef resolves "#/$defs/Name" and "#/definitions/Name". Only
// shallow, local pointers are handled — remote and deeper pointers stay
// untouched (JSON-pointer ~0/~1 escapes are rare in MCP tool names and are
// left unresolved by design).
func lookupLocalRef(defs map[string]any, ref string) (any, bool) {
	rest, ok := strings.CutPrefix(ref, "#/")
	if !ok {
		return nil, false
	}
	rest = strings.TrimPrefix(rest, "$defs/")
	rest = strings.TrimPrefix(rest, "definitions/")
	if rest == "" || strings.ContainsAny(rest, "/~") {
		return nil, false
	}
	v, ok := defs[rest]
	return v, ok
}

// deepCopyJSON clones a JSON-shaped value so two $ref sites never share one
// expanded subtree (an in-place edit through one would corrupt the other).
func deepCopyJSON(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out any
	if json.Unmarshal(b, &out) != nil {
		return nil
	}
	return out
}
