// Package dashboard is a stand-in for a real service, so the contract test next to it has
// something to check.
package dashboard

import (
	"encoding/json"
	"net/http"
)

type request struct {
	UserID string `json:"user_id"`
	Locale string `json:"locale,omitempty"`
}

type widget struct {
	ID    int      `json:"id"`
	Label string   `json:"label"`
	Tags  []string `json:"tags,omitempty"`
}

type response struct {
	UserID    string   `json:"user_id"`
	Widgets   []widget `json:"widgets"`
	Generated int      `json:"generated"`
}

// Handler implements POST /api/v1/dashboard.
func Handler(w http.ResponseWriter, r *http.Request) {
	var req request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.UserID == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid_user"})
		return
	}
	json.NewEncoder(w).Encode(response{
		UserID:    req.UserID,
		Widgets:   []widget{{ID: 1, Label: "Sales"}, {ID: 2, Label: "Alerts", Tags: []string{"ops"}}},
		Generated: 1700000000,
	})
}
