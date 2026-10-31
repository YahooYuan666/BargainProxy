package trae

import (
	"encoding/json"
)

// mergeTopLevel adds keys to a JSON object body without disturbing anything
// else the driver wrote.
func mergeTopLevel(raw []byte, extra map[string]any) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	for k, v := range extra {
		doc[k] = v
	}
	return json.Marshal(doc)
}
