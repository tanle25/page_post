// Command facebook-page-mcp is an MCP server for Facebook Pages built on the
// official Graph API (graph.facebook.com) with access tokens from a Meta app.
// It never touches cookies or the facebook.com web app, so it is the sanctioned way to publish to
// Pages the user manages.
//
// It runs in two modes:
//
//   - Locally it speaks MCP over stdio. Configuration lives in
//     ~/.facebook-go/page-config.json (FBP_CONFIG overrides the path):
//     {"app_id": "...", "app_secret": "...", "user_token": "..."}
//     and state in ~/.facebook-go. Register with Claude Code:
//     claude mcp add facebook-page -- go run .
//
//   - On Vercel (VERCEL is set) or with FBP_HTTP_ADDR it serves stateless
//     Streamable HTTP MCP at /api/mcp, guarded by FBP_AUTH_TOKEN. The config
//     comes from FB_APP_ID, FB_APP_SECRET and FB_USER_TOKEN, and state is
//     kept in Upstash Redis (UPSTASH_REDIS_REST_URL/TOKEN or KV_REST_API_*).
//
// fbp_refresh_pages exchanges the user token for a long-lived one (when the
// app secret is set) and stores a Page token for every managed Page. Tokens
// are never returned by any tool. Every write attempt is logged.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	serverName      = "facebook-page"
	serverVersion   = "0.1.0"
	protocolVersion = "2025-06-18"
	callTimeout     = 5 * time.Minute
	// Vercel ends a request after 300 s; stop first so the reply gets out.
	hostedCallTimeout = 280 * time.Second
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type callResult struct {
	Content []textContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

type tool struct {
	Name        string
	Description string
	Schema      map[string]any
	Run         func(ctx context.Context, s *server, args json.RawMessage) (any, error)
}

type server struct {
	gr      *graph
	store   *store
	hist    *history
	tools   []tool
	remote  bool          // hosted over HTTP: local image files are not available
	timeout time.Duration // per tool call

	outMu sync.Mutex
	out   *json.Encoder

	// writeMu serialises writes within this process. The write stop itself
	// is kept in the backend so every serverless instance honours it.
	writeMu sync.Mutex
}

func main() {
	log.SetOutput(os.Stderr)
	log.SetPrefix("facebook-page-mcp: ")

	remote := os.Getenv("VERCEL") != "" || os.Getenv("FBP_HTTP_ADDR") != ""
	var b backend
	if rb := redisFromEnv(os.Getenv); rb != nil {
		b = rb
	} else if remote {
		log.Fatal("no storage: connect an Upstash Redis database (UPSTASH_REDIS_REST_URL and UPSTASH_REDIS_REST_TOKEN, or KV_REST_API_URL and KV_REST_API_TOKEN)")
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			log.Fatal(err)
		}
		dir := filepath.Join(home, ".facebook-go")
		cfgPath := os.Getenv("FBP_CONFIG")
		if cfgPath == "" {
			cfgPath = filepath.Join(dir, "page-config.json")
		}
		b = newFileBackend(dir, cfgPath)
	}
	s := newServer(b, defaultGraphBase)
	s.remote = remote
	if remote {
		s.timeout = hostedCallTimeout
	}

	if !remote {
		s.out = json.NewEncoder(os.Stdout)
		if err := s.serve(os.Stdin); err != nil {
			log.Fatal(err)
		}
		return
	}
	addr := os.Getenv("FBP_HTTP_ADDR")
	if addr == "" {
		port := os.Getenv("PORT")
		if port == "" {
			port = "3000"
		}
		addr = ":" + port
	}
	auth := os.Getenv("FBP_AUTH_TOKEN")
	if len(auth) < minAuthTokenLen {
		log.Fatalf("FBP_AUTH_TOKEN must be set to a random secret of at least %d characters", minAuthTokenLen)
	}
	log.Printf("serving MCP on %s/api/mcp", addr)
	log.Fatal(http.ListenAndServe(addr, s.httpHandler(auth)))
}

func newServer(b backend, graphBase string) *server {
	return &server{gr: newGraph(graphBase), store: &store{b: b}, hist: &history{b: b}, tools: tools, timeout: callTimeout}
}

func (s *server) serve(in io.Reader) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var wg sync.WaitGroup
	defer wg.Wait()
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			s.send(response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: -32700, Message: err.Error()}})
			continue
		}
		if req.Method == "tools/call" {
			wg.Add(1)
			go func() {
				defer wg.Done()
				s.handle(req)
			}()
			continue
		}
		s.handle(req)
	}
	return sc.Err()
}

func (s *server) handle(req request) {
	if resp := s.dispatch(context.Background(), req); resp != nil {
		s.send(*resp)
	}
}

// dispatch answers one JSON-RPC message; notifications return nil.
func (s *server) dispatch(ctx context.Context, req request) *response {
	reply := func(result any, rerr *rpcError) *response {
		if len(req.ID) == 0 {
			if rerr == nil {
				return nil
			}
			req.ID = json.RawMessage("null")
		}
		return &response{JSONRPC: "2.0", ID: req.ID, Result: result, Error: rerr}
	}
	switch req.Method {
	case "initialize":
		return reply(map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": serverName, "version": serverVersion},
		}, nil)
	case "ping":
		return reply(map[string]any{}, nil)
	case "tools/list":
		list := make([]map[string]any, 0, len(s.tools))
		for _, t := range s.tools {
			list = append(list, map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.Schema})
		}
		return reply(map[string]any{"tools": list}, nil)
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return reply(nil, &rpcError{Code: -32602, Message: err.Error()})
		}
		for _, t := range s.tools {
			if t.Name == p.Name {
				ctx, cancel := context.WithTimeout(ctx, s.timeout)
				out, err := t.Run(ctx, s, p.Arguments)
				cancel()
				return reply(toResult(out, err), nil)
			}
		}
		return reply(nil, &rpcError{Code: -32602, Message: "unknown tool: " + p.Name})
	default:
		if len(req.ID) > 0 {
			return reply(nil, &rpcError{Code: -32601, Message: "method not found: " + req.Method})
		}
		return nil
	}
}

func toResult(out any, err error) callResult {
	if err != nil {
		var ae *apiError
		if !errors.As(err, &ae) {
			ae = &apiError{Code: "internal_error", Message: err.Error()}
		}
		b, _ := json.Marshal(ae)
		return callResult{Content: []textContent{{Type: "text", Text: string(b)}}, IsError: true}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return toResult(nil, err)
	}
	return callResult{Content: []textContent{{Type: "text", Text: string(b)}}}
}

func (s *server) send(resp response) {
	s.outMu.Lock()
	defer s.outMu.Unlock()
	if err := s.out.Encode(resp); err != nil {
		log.Printf("write response: %v", err)
	}
}

// noteError trips the write guard on token and account-level failures.
func (s *server) noteError(err error) error {
	var ae *apiError
	if errors.As(err, &ae) && writeStop(ae.Code) {
		s.setTrip(ae)
	}
	return err
}

// trip returns the stored write stop, if any. An unreadable store counts as
// stopped, so a storage outage cannot let writes slip past a stop.
func (s *server) trip() *apiError {
	b, err := s.store.b.loadTrip()
	if err != nil {
		return &apiError{Code: "storage_error", Message: "cannot read the write stop: " + err.Error()}
	}
	if len(b) == 0 {
		return nil
	}
	var ae apiError
	if json.Unmarshal(b, &ae) != nil || ae.Code == "" {
		return nil
	}
	return &ae
}

func (s *server) setTrip(ae *apiError) {
	var b []byte
	if ae != nil {
		b, _ = json.Marshal(ae)
	}
	if err := s.store.b.saveTrip(b); err != nil {
		log.Printf("write stop: %v", err)
	}
}

func decodeArgs(raw json.RawMessage, v any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errf("invalid_input", "%v", err)
	}
	return nil
}
