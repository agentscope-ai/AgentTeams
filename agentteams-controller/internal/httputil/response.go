package httputil

import (
	"encoding/json"
	"log"
	"net/http"
)

// ErrorResponse is the standard JSON error response.
type ErrorResponse struct {
	Message string `json:"message"`
}

// WriteJSON writes a JSON response with the given status code.
func WriteJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("[WARN] failed to write JSON response: %v", err)
	}
}

// WriteError writes a JSON error response.
func WriteError(w http.ResponseWriter, status int, message string) {
	WriteJSON(w, status, ErrorResponse{Message: message})
}
