package main

import (
	"encoding/json"
	"errors"
	"log"
	"time"
)

// history records one entry per write attempt (successful or not), so posts
// survive the Claude session and can later be loaded into a database or
// shown in a UI. It lives in the backend: a JSONL file locally, a Redis list
// on Vercel.

type historyEntry struct {
	Time         string   `json:"time"`
	Action       string   `json:"action"` // create_post, update_post, delete_post, reply_comment, hide_comment, delete_comment
	Status       string   `json:"status"` // ok or the error code
	Error        string   `json:"error,omitempty"`
	PageID       string   `json:"page_id,omitempty"`
	PageName     string   `json:"page_name,omitempty"`
	TargetID     string   `json:"target_id,omitempty"` // post or comment acted on
	Message      string   `json:"message,omitempty"`
	Images       []string `json:"images,omitempty"`
	PhotoIDs     []string `json:"photo_ids,omitempty"`
	ResultID     string   `json:"result_id,omitempty"`
	Permalink    string   `json:"permalink,omitempty"`
	ScheduledFor string   `json:"scheduled_for,omitempty"`
}

type history struct{ b backend }

func (h *history) add(e historyEntry, err error) {
	if e.Time == "" {
		e.Time = time.Now().Format(time.RFC3339)
	}
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) {
			e.Status, e.Error = ae.Code, ae.Message
		} else {
			e.Status, e.Error = "internal_error", err.Error()
		}
	} else if e.Status == "" {
		e.Status = "ok"
	}
	b, merr := json.Marshal(e)
	if merr != nil {
		return
	}
	if err := h.b.appendHistory(b); err != nil {
		log.Printf("history: %v", err)
	}
}

// query returns entries newest first, filtered by the non-empty fields.
func (h *history) query(action, pageID, status string, limit int) ([]historyEntry, error) {
	lines, err := h.b.historyLines()
	if err != nil {
		return nil, err
	}
	out := []historyEntry{}
	for i := len(lines) - 1; i >= 0 && (limit <= 0 || len(out) < limit); i-- {
		var e historyEntry
		if json.Unmarshal(lines[i], &e) != nil {
			continue
		}
		if (action == "" || e.Action == action) && (pageID == "" || e.PageID == pageID) &&
			(status == "" || e.Status == status || (status == "error" && e.Status != "ok")) {
			out = append(out, e)
		}
	}
	return out, nil
}
