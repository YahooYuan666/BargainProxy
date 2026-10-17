// HTTP surface for service management (see service.go for semantics).
package panel

import (
	"net/http"
)

type serviceToggleRequest struct {
	Enabled bool `json:"enabled"`
}

// handleServiceAutostart toggles boot autostart (panel switch → scheduled
// task create/delete + persisted preference).
func (p *Panel) handleServiceAutostart(w http.ResponseWriter, r *http.Request) {
	var req serviceToggleRequest
	if !decode(w, r, &req) {
		return
	}
	if err := p.SetAutostart(req.Enabled); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, p.ServiceView())
}

// handleServiceWatchdog toggles the every-5-minutes watchdog task.
func (p *Panel) handleServiceWatchdog(w http.ResponseWriter, r *http.Request) {
	var req serviceToggleRequest
	if !decode(w, r, &req) {
		return
	}
	if err := p.SetWatchdog(req.Enabled); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, p.ServiceView())
}

// handleServiceGet reports preferences and the real scheduled-task states.
func (p *Panel) handleServiceGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, p.ServiceView())
}
