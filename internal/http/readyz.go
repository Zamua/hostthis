package http

import (
	"encoding/json"
	"net/http"
)

func (s *Server) serveReadyz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(struct {
		Ready bool `json:"ready"`
	}{Ready: true})
}
