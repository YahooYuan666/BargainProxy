package streamx

// ToolFolder folds streamed openai-shaped tool_call fragments into complete
// calls (BP-320).
//
// # The bug it fixes
//
// Fragments fold by upstream `index`; a fragment with a NEW id at an index
// already in use is a NEW call, not more of the open one. Folding by index
// alone ran two calls together into malformed arguments `{"a"}{"b"` (upstream
// field case: magpie-community/plugins qoder 0.2.65 #930; magpie 835439eb).
// Fragments with no id — or the same id — continue the open call, which is
// the normal openai streaming convention.
//
// # Renumbering
//
// Every call is numbered from 0 in START order (magpie 873b4987). Downstream
// block writers key on ToolIndex (the anthropic lane buffers tool blocks by
// index, BP-70), so a second call sharing an upstream index must be handed
// downstream under a fresh sequential index — the folder owns that remap.
type ToolFolder struct {
	byID    map[string]int // upstream call id → slot; only fragments WITH an id route here
	byIndex map[int]int    // upstream index → slot of the call LAST opened there
	calls   []FoldedCall
}

// FoldedCall is one complete call in start order; Slot IS its downstream
// index (0, 1, 2, … by start order).
type FoldedCall struct {
	Slot int
	ID   string
	Name string
	Args string
}

// AddResult tells the driver what to emit for one fragment.
type AddResult struct {
	Slot    int    // downstream (renumbered) index
	Started bool   // this fragment opened a NEW call → emit EvToolStart
	ID      string // the call's id (first fragment's value for a new call)
	Name    string // the name accumulated so far (start event carries it)
}

// NewToolFolder returns an empty folder.
func NewToolFolder() *ToolFolder {
	return &ToolFolder{byID: map[string]int{}, byIndex: map[int]int{}}
}

// Add folds one streamed fragment (one element of delta.tool_calls).
//
// Split condition (magpie 835439eb): the fragment carries an id, the open
// call at this index ALSO carries one, and they differ → new call. An id
// arriving for an open ID-LESS call is the late-id convention (qoder's fill
// semantics) — it fills the open call instead of splitting.
func (f *ToolFolder) Add(index int, id, nameDelta, argsDelta string) AddResult {
	slot := -1
	if id != "" {
		if s, ok := f.byID[id]; ok {
			slot = s
		}
	}
	if slot < 0 {
		if s, ok := f.byIndex[index]; ok {
			if id == "" {
				slot = s
			} else if f.calls[s].ID == "" {
				// Late id for the open id-less call: fill, don't split.
				f.calls[s].ID = id
				f.byID[id] = s
				slot = s
				id = ""
			}
			// else: an id'd fragment against an open call with a DIFFERENT id
			// → fall through and open a new call (the BP-320 split).
		}
	}
	started := slot < 0
	if started {
		slot = len(f.calls)
		f.calls = append(f.calls, FoldedCall{Slot: slot, ID: id})
		if id != "" {
			f.byID[id] = slot
		}
		// This upstream index now points at the newest call: id-less
		// fragments that follow belong to it (the streaming convention).
		f.byIndex[index] = slot
	}
	if nameDelta != "" {
		f.calls[slot].Name += nameDelta
	}
	if argsDelta != "" {
		f.calls[slot].Args += argsDelta
	}
	return AddResult{Slot: slot, Started: started, ID: f.calls[slot].ID, Name: f.calls[slot].Name}
}

// List returns the folded calls in start order; Slot is the downstream index.
func (f *ToolFolder) List() []FoldedCall { return f.calls }
