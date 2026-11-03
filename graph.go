package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	defaultGraphBase    = "https://graph.facebook.com"
	defaultGraphVersion = "v23.0"
	minRequestGap       = 300 * time.Millisecond
)

// apiError is what every tool reports on failure. Code is a stable slug;
// GraphCode/Subcode/TraceID carry Facebook's own error for debugging.
type apiError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	GraphCode int    `json:"graph_code,omitempty"`
	Subcode   int    `json:"graph_subcode,omitempty"`
	TraceID   string `json:"fbtrace_id,omitempty"`
	Retryable bool   `json:"retryable,omitempty"`
}

func (e *apiError) Error() string { return e.Code + ": " + e.Message }

func errf(code, format string, args ...any) *apiError {
	return &apiError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// writeStop reports codes after which posting should pause: a broken token
// or Facebook pushing back on the account.
func writeStop(code string) bool {
	switch code {
	case "token_invalid", "feature_blocked", "rate_limited", "write_unknown":
		return true
	}
	return false
}

// classifyGraphError maps a Graph API error to a slug. See
// https://developers.facebook.com/docs/graph-api/guides/error-handling
func classifyGraphError(status int, code, sub int, msg string) string {
	switch {
	case code == 190 || code == 102 || code == 463 || code == 467:
		return "token_invalid"
	case code == 4 || code == 17 || code == 32 || code == 613 || code == 80001 || code == 80004:
		return "rate_limited"
	case code == 368:
		return "feature_blocked"
	case code == 10 || (code >= 200 && code <= 299):
		return "permission_denied"
	case code == 803 || (code == 100 && sub == 33):
		return "not_found"
	case code == 100 || code == 1:
		return "invalid_request"
	case code == 2 || status >= 500:
		return "unavailable"
	}
	return "graph_error"
}

type graph struct {
	base    string
	version string
	secret  string // app secret; adds appsecret_proof when set
	hc      *http.Client

	mu   sync.Mutex
	last time.Time
}

func newGraph(base string) *graph {
	return &graph{base: base, version: defaultGraphVersion, hc: &http.Client{Timeout: 2 * time.Minute}}
}

type upload struct {
	Field string
	Name  string
	Type  string
	Data  []byte
}

// call sends one Graph request. GET and DELETE carry params in the query;
// POST sends a form, or multipart when files are attached.
func (g *graph) call(ctx context.Context, method, path, token string, params url.Values, files ...upload) (map[string]any, error) {
	if params == nil {
		params = url.Values{}
	}
	g.mu.Lock()
	version, secret := g.version, g.secret
	g.mu.Unlock()
	if token != "" {
		params.Set("access_token", token)
		if secret != "" {
			m := hmac.New(sha256.New, []byte(secret))
			m.Write([]byte(token))
			params.Set("appsecret_proof", hex.EncodeToString(m.Sum(nil)))
		}
	}
	endpoint := strings.TrimRight(g.base, "/") + "/" + version + "/" + strings.TrimLeft(path, "/")

	var body io.Reader
	contentType := ""
	switch {
	case method != http.MethodPost:
		endpoint += "?" + params.Encode()
	case len(files) > 0:
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		for k, vs := range params {
			for _, v := range vs {
				mw.WriteField(k, v)
			}
		}
		for _, f := range files {
			h := textproto.MIMEHeader{}
			h.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, f.Field, f.Name))
			h.Set("Content-Type", f.Type)
			w, err := mw.CreatePart(h)
			if err != nil {
				return nil, err
			}
			w.Write(f.Data)
		}
		mw.Close()
		body, contentType = &buf, mw.FormDataContentType()
	default:
		body, contentType = strings.NewReader(params.Encode()), "application/x-www-form-urlencoded"
	}

	g.pace(ctx)
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := g.hc.Do(req)
	if err != nil {
		if method == http.MethodPost {
			return nil, &apiError{Code: "write_unknown", Message: "request failed before a reply; check the Page before retrying: " + err.Error()}
		}
		return nil, &apiError{Code: "unavailable", Message: err.Error(), Retryable: true}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, &apiError{Code: "unavailable", Message: err.Error(), Retryable: true}
	}
	return parseGraph(resp.StatusCode, raw, method == http.MethodPost)
}

func parseGraph(status int, raw []byte, write bool) (map[string]any, error) {
	var out map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		// Some endpoints answer a bare true/false.
		var b bool
		if json.Unmarshal(raw, &b) == nil {
			return map[string]any{"success": b}, nil
		}
		code := "decode_error"
		if write && status < 400 {
			code = "write_unknown"
		}
		snippet := string(raw)
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return nil, &apiError{Code: code, Message: fmt.Sprintf("HTTP %d, undecodable reply: %s", status, snippet)}
	}
	if e, ok := out["error"].(map[string]any); ok {
		code, sub := toInt(e["code"]), toInt(e["error_subcode"])
		msg := str(e["message"])
		if u := str(e["error_user_msg"]); u != "" {
			msg += " (" + u + ")"
		}
		slug := classifyGraphError(status, code, sub, msg)
		return nil, &apiError{
			Code: slug, Message: msg, GraphCode: code, Subcode: sub, TraceID: str(e["fbtrace_id"]),
			Retryable: slug == "unavailable" || e["is_transient"] == true,
		}
	}
	if status >= 400 {
		return nil, &apiError{Code: "graph_error", Message: fmt.Sprintf("HTTP %d", status)}
	}
	return out, nil
}

func (g *graph) pace(ctx context.Context) {
	g.mu.Lock()
	wait := time.Until(g.last.Add(minRequestGap))
	if wait < 0 {
		wait = 0
	}
	g.last = time.Now().Add(wait)
	g.mu.Unlock()
	if wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
		}
	}
}

// page is one result page of a Graph edge.
type page struct {
	Data       []any  `json:"data"`
	NextCursor string `json:"next_cursor,omitempty"`
}

func edgePage(out map[string]any) page {
	p := page{Data: []any{}}
	if d, ok := out["data"].([]any); ok {
		p.Data = d
	}
	if paging, ok := out["paging"].(map[string]any); ok {
		if _, more := paging["next"]; more {
			if c, ok := paging["cursors"].(map[string]any); ok {
				p.NextCursor = str(c["after"])
			}
		}
	}
	return p
}

func str(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	}
	return ""
}

func toInt(v any) int {
	switch t := v.(type) {
	case json.Number:
		n, _ := t.Int64()
		return int(n)
	case float64:
		return int(t)
	}
	return 0
}
