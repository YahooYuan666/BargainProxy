package workbuddy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// BP-228 (misakano 96a33f13/046b1fdf): the upstream growth center removed
// Model_chat_GLM5.2, Buddy_App_QQ and create_canvas. The pipeline must skip
// what the account's live list no longer carries — dead codes earn nothing
// and read as automation. With a live list that matches our step vocabulary,
// unlisted steps are skipped and the listed ones still run.
func TestBP228GrowthPipelineSkipsRemovedTasks(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case growthTasksPath:
			// The account lists only chat_5 — our vocabulary matches, so
			// everything else is skippable; the three removed codes are
			// gated even without this fetch succeeding.
			fmt.Fprint(w, `{"code":0,"data":{"tasks":[{"task_code":"chat_5","current":0,"target":5}]}}`)
			return
		}
		// Every other endpoint: a plain business error. The steps record the
		// failure and the pipeline carries on — we only assert the skips.
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"code":400,"msg":"no"}`)
	}))
	defer up.Close()

	d := testDriver(up.URL)
	acc := testAccount()
	results, _ := d.RunGrowthPipeline(context.Background(), acc)
	byStep := map[string]growthStepResult{}
	for _, r := range results {
		byStep[r.Step] = r
	}
	removed := map[string]bool{"Model_chat_GLM5.2": true, "Buddy_App_QQ": true, "create_canvas": true}
	for step, r := range byStep {
		isRemoved := removed[step] || (step == "Buddy_App" && removed["Buddy_App_QQ"])
		wantSkip := isRemoved || step == "RichMeow_Chat" || step == "automation_1" ||
			step == "Library_read" || step == "template_5" || step == "playbook_prompt"
		if wantSkip {
			if !strings.Contains(r.Note, "skipped") {
				t.Fatalf("step %s must be skipped, got %+v", step, r)
			}
			if !r.OK {
				t.Fatalf("a skip is not a failure: %s = %+v", step, r)
			}
		}
	}
	// chat_5 is listed: it must have been ATTEMPTED (its report endpoint
	// answers 400, so the recorded result carries the failure, not a skip).
	if r := byStep["chat_5"]; r.OK || strings.Contains(r.Note, "skipped") {
		t.Fatalf("chat_5 is still listed and must have been attempted, got %+v", r)
	}
}

// The three confirmed removals stay gated even when the live-list fetch
// fails — the hard gate does not depend on the vocabulary check.
func TestBP228RemovedTasksGatedWithoutList(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"code":503,"msg":"down"}`)
	}))
	defer up.Close()

	d := testDriver(up.URL)
	results, _ := d.RunGrowthPipeline(context.Background(), testAccount())
	for _, r := range results {
		if r.Step == "Model_chat_GLM5.2" || r.Step == "Buddy_App" || r.Step == "create_canvas" {
			if !strings.Contains(r.Note, "skipped") {
				t.Fatalf("removed step %s must stay gated without a live list, got %+v", r.Step, r)
			}
		}
	}
}
