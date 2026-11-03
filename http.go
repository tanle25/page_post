package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// The hosted mode serves stateless Streamable HTTP: each POST carries one
// JSON-RPC message and gets one JSON reply. There is no session and no
// server-to-client stream, which suits serverless instances that come and go.

const (
	minAuthTokenLen = 24
	maxRequestBytes = 4 << 20 // Vercel rejects bodies over 4.5 MB anyway
)

func (s *server) httpHandler(authToken string) http.Handler {
	mux := http.NewServeMux()
	mcp := s.mcpHandler(authToken)
	mux.Handle("/api/mcp", mcp)
	mux.Handle("/mcp", mcp)
	// Public, unauthenticated: Meta links to these from the app settings.
	mux.HandleFunc("/privacy", servePolicy(privacyTmpl))
	mux.HandleFunc("/data-deletion", servePolicy(deletionTmpl))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"name": serverName, "version": serverVersion, "mcp_endpoint": "/api/mcp", "privacy_policy": "/privacy", "data_deletion": "/data-deletion"})
	})
	return mux
}

func (s *server) mcpHandler(authToken string) http.HandlerFunc {
	want := sha256.Sum256([]byte(authToken))
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			// No standalone SSE stream and no sessions to delete.
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		got := sha256.Sum256([]byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")))
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="facebook-page"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "reading request: "+err.Error(), http.StatusBadRequest)
			return
		}
		var req request
		if err := json.Unmarshal(body, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: -32700, Message: err.Error()}})
			return
		}
		resp := s.dispatch(context.WithoutCancel(r.Context()), req)
		if resp == nil {
			w.WriteHeader(http.StatusAccepted) // notification or client response
			return
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
