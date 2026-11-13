// Tool-schema shape rewriting for the Gemini functionDeclarations payload
// (BP-321). The allow-list in schema.go drops every key it does not know —
// including anyOf/oneOf/prefixItems/const — so tools written with modern JSON
// Schema reach the model as amputated stubs: unions vanish, tuples vanish,
// the model loses the constraints it is supposed to act on. magpie 65cb76ca
// field evidence (Copilot's Gemini, 2026-11-12): such shapes earn a 400 or a
// degraded tool; the fix is to REWRITE each shape into the nearest schema the
// endpoint accepts, keeping what the model can act on.
//
// One deliberate divergence from magpie: it leaves nested unions in place
// because ITS endpoint accepts them (#4374). Our endpoint's acceptance of
// nested anyOf is unverified, so every shape here is rewritten INTO the
// allow-listed vocabulary (type/properties/required/enum/items/nullable/
// description) — nothing new is sent, nothing is guessed.
//
// Shapes handled (all recursed children-first):
//   - union (anyOf/oneOf), branches null-apart: that one branch + nullable;
//   - union, string-enum branches: enum = union of members, type STRING;
//   - union, object branches: one object with the properties joined (first
//     wins on conflicts), required = the branches' intersection;
//   - union, mixed/untyped branches: union dropped, description kept, type
//     inferred from what survives (properties→OBJECT, enum→STRING,
//     items→ARRAY);
//   - enum on an array node: moved onto items, else dropped;
//   - prefixItems / items-as-list: the shared schema, else the first;
//   - OBJECT with no properties: properties {} added;
//   - untyped node: type inferred (properties→OBJECT, enum→STRING,
//     items→ARRAY) — same job as magpie's geminiTypes.
package antigravity

import (
	"encoding/json"
	"strings"
)

// rewriteGeminiShapes rewrites the union/tuple shapes in a raw schema tree,
// children first, returning a tree the allow-list pass can express fully.
func rewriteGeminiShapes(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	// Children first, so a union is judged on branches already rewritten.
	if props, ok := m["properties"].(map[string]any); ok {
		next := make(map[string]any, len(props))
		for k, pv := range props {
			next[k] = rewriteGeminiShapes(pv)
		}
		m["properties"] = next
	}
	for _, key := range []string{"items", "additionalProperties"} {
		if child, ok := m[key]; ok {
			m[key] = rewriteGeminiShapes(child)
		}
	}
	rewriteGeminiEnumOnArray(m)
	rewriteGeminiTuple(m)
	rewriteGeminiUnion(m)
	inferGeminiType(m)
	addEmptyProperties(m)
	return m
}

// rewriteGeminiUnion collapses anyOf/oneOf into one allow-listed node.
func rewriteGeminiUnion(m map[string]any) {
	raw, ok := firstOf(m, "anyOf", "oneOf")
	if !ok {
		return
	}
	branches, ok := raw.([]any)
	if !ok || len(branches) == 0 {
		return
	}
	delete(m, "anyOf")
	delete(m, "oneOf")
	// A union that is null plus ONE other branch: that branch + nullable.
	var real []map[string]any
	nullable := false
	for _, b := range branches {
		bm, ok := b.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := bm["type"].(string); strings.EqualFold(t, "null") {
			nullable = true
			continue
		}
		if len(bm) == 0 { // an empty schema accepts anything: skip it
			continue
		}
		real = append(real, bm)
	}
	if len(real) == 1 {
		mergeGeminiBranch(m, real[0])
		if nullable {
			m["nullable"] = true
		}
		return
	}
	if len(real) == 0 {
		if nullable {
			m["nullable"] = true
		}
		return
	}
	mergeGeminiBranches(m, real, nullable)
}

// mergeGeminiBranch folds one surviving branch into the node (description and
// the branch's own fields), preserving any description the node already had.
func mergeGeminiBranch(m map[string]any, branch map[string]any) {
	if desc, ok := m["description"].(string); ok && desc != "" {
		branch = withDescription(branch, desc)
	}
	for k, v := range branch {
		m[k] = v
	}
}

// mergeGeminiBranches merges several branches into ONE node the allow-list
// can express: object branches join properties (first wins) with the
// required-intersection; string-enum branches join their members; anything
// else degrades to a description-carrying node with an inferred type.
func mergeGeminiBranches(m map[string]any, branches []map[string]any, nullable bool) {
	desc := descOf(m)
	types := map[string]bool{}
	for _, b := range branches {
		if t, ok := b["type"].(string); ok && t != "" {
			types[strings.ToUpper(t)] = true
		}
	}
	joined := map[string]any{}
	switch {
	case len(types) == 1 && types["OBJECT"]:
		props := map[string]any{}
		var req []any
		first := true
		for _, b := range branches {
			bp, _ := b["properties"].(map[string]any)
			for k, v := range bp {
				if _, dup := props[k]; !dup {
					props[k] = v
				}
			}
			if first {
				for _, r := range toList(b["required"]) {
					if rs, ok := r.(string); ok {
						req = append(req, rs)
					}
				}
				first = false
			} else {
				req = intersect(req, toList(b["required"]))
			}
		}
		joined["type"] = "OBJECT"
		joined["properties"] = props
		if len(req) > 0 {
			joined["required"] = req
		}
	case len(types) == 1 && types["STRING"] && allHaveEnum(branches):
		joined["type"] = "STRING"
		joined["enum"] = joinEnums(branches)
	default:
		// Mixed or untyped: the union goes, the description stays, and the
		// type comes from whatever the merged body still carries.
		props := map[string]any{}
		var enums []any
		var items any
		hasProps := false
		for _, b := range branches {
			if bp, ok := b["properties"].(map[string]any); ok && len(bp) > 0 {
				hasProps = true
				for k, v := range bp {
					if _, dup := props[k]; !dup {
						props[k] = v
					}
				}
			}
			if bHasEnum, ok := b["enum"].([]any); ok {
				enums = append(enums, bHasEnum...)
			}
			if bItems, ok := b["items"]; ok && items == nil {
				items = bItems
			}
		}
		switch {
		case hasProps:
			joined["type"] = "OBJECT"
			joined["properties"] = props
		case len(enums) > 0:
			joined["type"] = "STRING"
			joined["enum"] = enums
		case items != nil:
			joined["type"] = "ARRAY"
			joined["items"] = items
		}
	}
	for k := range m {
		delete(m, k)
	}
	mergeGeminiBranch(m, joined)
	if desc != "" {
		m["description"] = desc
	}
	if nullable {
		m["nullable"] = true
	}
}

// rewriteGeminiEnumOnArray: an enum on an ARRAY node moves onto items when
// those are strings, else it is dropped (magpie 65cb76ca shape 4).
func rewriteGeminiEnumOnArray(m map[string]any) {
	raw, ok := m["enum"].([]any)
	if !ok {
		return
	}
	t, _ := m["type"].(string)
	isArray := strings.EqualFold(t, "ARRAY")
	items, hasItems := m["items"].(map[string]any)
	if !isArray && !hasItems {
		return
	}
	strs := make([]any, 0, len(raw))
	for _, e := range raw {
		if s, ok := e.(string); ok {
			strs = append(strs, s)
		}
	}
	if len(strs) == 0 {
		delete(m, "enum") // non-string members: nothing expressible, drop
		return
	}
	delete(m, "enum") // moved onto items — the node-level copy must go
	if !hasItems {
		m["items"] = map[string]any{"type": "STRING", "enum": strs}
	} else if _, hasType := items["type"]; !hasType {
		items["type"] = "STRING"
		items["enum"] = strs
	} else {
		items["enum"] = strs
	}
}

// rewriteGeminiTuple: prefixItems (or items as a LIST) collapses to the one
// schema the positions share, else the first (magpie 65cb76ca shape 6; the
// anyOf-of-items form its endpoint takes is not in our allow-list, so the
// first position stands in).
func rewriteGeminiTuple(m map[string]any) {
	if raw, ok := m["prefixItems"].([]any); ok {
		delete(m, "prefixItems")
		if item := sharedOrFirst(raw); item != nil {
			m["items"] = item
		}
		return
	}
	if raw, ok := m["items"].([]any); ok {
		if item := sharedOrFirst(raw); item != nil {
			m["items"] = item
		} else {
			delete(m, "items")
		}
	}
}

// inferGeminiType fills a missing type from the node's own body (magpie's
// geminiTypes job): properties→OBJECT, enum→STRING, items→ARRAY.
func inferGeminiType(m map[string]any) {
	if t, ok := m["type"]; ok && t != nil && t != "" {
		return
	}
	switch {
	case hasNonEmpty(m, "properties"):
		m["type"] = "OBJECT"
	case hasNonEmpty(m, "enum"):
		m["type"] = "STRING"
	case hasNonEmpty(m, "items"):
		m["type"] = "ARRAY"
	}
}

// addEmptyProperties: an OBJECT with no properties becomes properties {} —
// the empty-but-typed form the endpoint takes (magpie 65cb76ca shape 1).
func addEmptyProperties(m map[string]any) {
	if t, _ := m["type"].(string); !strings.EqualFold(t, "OBJECT") {
		return
	}
	if _, ok := m["properties"]; !ok {
		m["properties"] = map[string]any{}
	}
}

// --- small helpers ---

func firstOf(m map[string]any, keys ...string) (any, bool) {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			return v, true
		}
	}
	return nil, false
}

func hasNonEmpty(m map[string]any, key string) bool {
	switch v := m[key].(type) {
	case map[string]any:
		return len(v) > 0
	case []any:
		return len(v) > 0
	}
	return false
}

func toList(v any) []any {
	if l, ok := v.([]any); ok {
		return l
	}
	return nil
}

func intersect(a, b []any) []any {
	set := map[string]bool{}
	for _, v := range b {
		if s, ok := v.(string); ok {
			set[s] = true
		}
	}
	var out []any
	for _, v := range a {
		if s, ok := v.(string); ok && set[s] {
			out = append(out, s)
		}
	}
	return out
}

func allHaveEnum(branches []map[string]any) bool {
	for _, b := range branches {
		if _, ok := b["enum"].([]any); !ok {
			return false
		}
	}
	return true
}

func joinEnums(branches []map[string]any) []any {
	seen := map[string]bool{}
	var out []any
	for _, b := range branches {
		for _, e := range toList(b["enum"]) {
			s, ok := e.(string)
			if !ok || seen[s] {
				continue
			}
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func descOf(m map[string]any) string {
	d, _ := m["description"].(string)
	return d
}

func withDescription(branch map[string]any, desc string) map[string]any {
	next := make(map[string]any, len(branch)+1)
	for k, v := range branch {
		next[k] = v
	}
	next["description"] = desc
	return next
}

// sharedOrFirst returns the one schema every tuple position shares, else the
// first (positions that differ lose their per-position fidelity — the
// allow-list has no way to express a union here).
func sharedOrFirst(list []any) any {
	if len(list) == 0 {
		return nil
	}
	first, _ := list[0].(map[string]any)
	if first == nil {
		return nil
	}
	if len(list) == 1 {
		return first
	}
	canonical := strings.TrimSpace(marshalJSON(first))
	for _, item := range list[1:] {
		im, ok := item.(map[string]any)
		if !ok || strings.TrimSpace(marshalJSON(im)) != canonical {
			return first
		}
	}
	return first
}

// marshalJSON is the canonical-form helper for sharedOrFirst's comparison.
func marshalJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
