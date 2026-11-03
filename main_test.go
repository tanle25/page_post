package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A 1x1 PNG.
var pngBytes = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89\x00\x00\x00\rIDATx\x9cc\xf8\x0f\x00\x00\x01\x01\x00\x05\x18\xd8N\x00\x00\x00\x00IEND\xaeB`\x82")

type fakeGraph struct {
	mu    sync.Mutex
	calls []string
	forms []map[string][]string
}

func (f *fakeGraph) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/"+defaultGraphVersion)
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		r.ParseMultipartForm(1 << 20)
	} else {
		r.ParseForm()
	}
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+path)
	f.forms = append(f.forms, r.Form)
	f.mu.Unlock()

	tok := r.Form.Get("access_token")
	if r.Form.Get("appsecret_proof") == "" && path != "/oauth/access_token" {
		http.Error(w, `{"error":{"message":"missing proof","code":100}}`, 400)
		return
	}
	switch {
	case path == "/oauth/access_token" && r.Form.Get("code") != "":
		if r.Form.Get("code") != "GOODCODE" || !strings.HasSuffix(r.Form.Get("redirect_uri"), "/oauth/callback") {
			http.Error(w, `{"error":{"message":"bad code","code":100}}`, 400)
			return
		}
		fmt.Fprint(w, `{"access_token":"OAUTH-SHORT","token_type":"bearer"}`)
	case path == "/oauth/access_token" && r.Form.Get("fb_exchange_token") == "OAUTH-SHORT":
		fmt.Fprint(w, `{"access_token":"LONG-BDS","token_type":"bearer"}`)
	case path == "/oauth/access_token":
		fmt.Fprint(w, `{"access_token":"LONG-USER","token_type":"bearer"}`)
	case path == "/me" && tok == "LONG-USER":
		fmt.Fprint(w, `{"id":"42","name":"Tester"}`)
	case path == "/me" && tok == "LONG-BDS":
		fmt.Fprint(w, `{"id":"43","name":"Chủ Nhà Đất"}`)
	case path == "/me/accounts" && tok == "LONG-BDS":
		fmt.Fprint(w, `{"data":[{"id":"888","name":"Nhà Đất Thanh Hóa","category":"Real Estate","tasks":["CREATE_CONTENT"],"access_token":"PAGE-BDS"}]}`)
	case path == "/me/accounts" && tok == "LONG-USER":
		fmt.Fprint(w, `{"data":[{"id":"777","name":"Tre Shop","category":"Shopping","tasks":["CREATE_CONTENT"],"access_token":"PAGE-TOKEN"}],"paging":{"cursors":{"after":"x"}}}`)
	case path == "/777/photos" && tok == "PAGE-TOKEN":
		hasFile := r.MultipartForm != nil && len(r.MultipartForm.File["source"]) == 1
		if hasFile == (r.Form.Get("url") != "") || r.Form.Get("published") != "false" {
			http.Error(w, `{"error":{"message":"bad upload","code":100}}`, 400)
			return
		}
		fmt.Fprintf(w, `{"id":"p%d"}`, len(f.calls))
	case path == "/777/feed" && tok == "PAGE-TOKEN":
		fmt.Fprint(w, `{"id":"777_999"}`)
	case path == "/777_999":
		fmt.Fprint(w, `{"id":"777_999","permalink_url":"https://www.facebook.com/777/posts/999"}`)
	case path == "/777/published_posts":
		fmt.Fprint(w, `{"data":[{"id":"777_1","message":"hi","reactions":{"data":[],"summary":{"total_count":5}},"comments":{"data":[],"summary":{"total_count":2}},"shares":{"count":1}}],"paging":{"cursors":{"after":"C2"},"next":"https://x"}}`)
	default:
		http.Error(w, `{"error":{"message":"Error validating access token","type":"OAuthException","code":190,"fbtrace_id":"T"}}`, 400)
	}
}

func testServer(t *testing.T, fg *fakeGraph) *server {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "page-config.json")
	os.WriteFile(cfg, []byte(`{"app_id":"1","app_secret":"sec","user_token":"SHORT-USER"}`), 0o600)
	ts := httptest.NewServer(fg)
	t.Cleanup(ts.Close)
	return newServer(newFileBackend(dir, cfg), ts.URL)
}

func call(t *testing.T, s *server, name string, args any) (map[string]any, *apiError) {
	t.Helper()
	raw, _ := json.Marshal(args)
	for _, tl := range s.tools {
		if tl.Name == name {
			out, err := tl.Run(context.Background(), s, raw)
			if err != nil {
				var ae *apiError
				if !errors.As(err, &ae) {
					t.Fatalf("%s: %v", name, err)
				}
				return nil, ae
			}
			b, _ := json.Marshal(out)
			var m map[string]any
			json.Unmarshal(b, &m)
			return m, nil
		}
	}
	t.Fatalf("no tool %s", name)
	return nil, nil
}

func TestRefreshAndPost(t *testing.T) {
	fg := &fakeGraph{}
	s := testServer(t, fg)

	out, err := call(t, s, "fbp_refresh_pages", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(out); strings.Contains(string(b), "TOKEN") || strings.Contains(string(b), "LONG-USER") {
		t.Fatalf("token leaked: %s", b)
	}
	cfg, _ := os.ReadFile(s.store.b.(*fileBackend).configPath)
	if !strings.Contains(string(cfg), "LONG-USER") || !strings.Contains(string(cfg), `"app_id"`) {
		t.Fatalf("config not updated: %s", cfg)
	}

	img := filepath.Join(t.TempDir(), "a.png")
	os.WriteFile(img, pngBytes, 0o600)
	out, err = call(t, s, "fbp_create_post", map[string]any{"page_id": "777", "message": "dòng 1\ndòng 2", "image_paths": []string{img, img}})
	if err != nil {
		t.Fatal(err)
	}
	if out["post_id"] != "777_999" || out["permalink"] != "https://www.facebook.com/777/posts/999" {
		t.Fatalf("post result %v", out)
	}
	feed := fg.forms[len(fg.forms)-2]
	if feed["attached_media[1]"] == nil || !strings.Contains(feed["attached_media[0]"][0], "media_fbid") || feed["message"][0] != "dòng 1\ndòng 2" {
		t.Fatalf("feed form %v", feed)
	}

	hist, _ := s.hist.query("create_post", "", "", 0)
	if len(hist) != 1 || hist[0].Status != "ok" || hist[0].PageName != "Tre Shop" || len(hist[0].PhotoIDs) != 2 {
		t.Fatalf("history %+v", hist)
	}

	posts, err := call(t, s, "fbp_get_posts", map[string]any{"page_id": "777"})
	if err != nil {
		t.Fatal(err)
	}
	p := posts["data"].([]any)[0].(map[string]any)
	if p["reactions_count"] != float64(5) || p["comments_count"] != float64(2) || p["shares_count"] != float64(1) || posts["next_cursor"] != "C2" {
		t.Fatalf("posts %v", posts)
	}
}

func TestCreatePostRequiresImages(t *testing.T) {
	fg := &fakeGraph{}
	s := testServer(t, fg)
	if _, err := call(t, s, "fbp_create_post", map[string]any{"page_id": "777", "message": "x", "image_paths": []string{}}); err == nil || err.Code != "invalid_input" {
		t.Fatalf("want invalid_input, got %v", err)
	}
	txt := filepath.Join(t.TempDir(), "a.txt")
	os.WriteFile(txt, []byte("hello"), 0o600)
	if _, err := call(t, s, "fbp_create_post", map[string]any{"page_id": "777", "message": "x", "image_paths": []string{txt}}); err == nil || err.Code != "invalid_input" {
		t.Fatalf("want invalid_input for text file, got %v", err)
	}
	if len(fg.calls) != 0 {
		t.Fatalf("Facebook was called: %v", fg.calls)
	}
}

func TestTokenErrorStopsWrites(t *testing.T) {
	fg := &fakeGraph{}
	s := testServer(t, fg)
	if _, err := call(t, s, "fbp_refresh_pages", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	// Unknown object -> fake answers code 190.
	if _, err := call(t, s, "fbp_delete_post", map[string]any{"page_id": "777", "post_id": "bad"}); err == nil || err.Code != "token_invalid" {
		t.Fatalf("want token_invalid, got %v", err)
	}
	if _, err := call(t, s, "fbp_delete_post", map[string]any{"page_id": "777", "post_id": "777_999"}); err == nil || err.Code != "write_stopped" {
		t.Fatalf("want write_stopped, got %v", err)
	}
	if _, err := call(t, s, "fbp_status", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if s.trip() != nil {
		t.Fatal("status with a valid token should clear the stop")
	}
}

func TestParseSchedule(t *testing.T) {
	now := time.Date(2026, 10, 31, 10, 0, 0, 0, ict)
	got, err := parseSchedule("2026-10-31 20:30", now)
	if err != nil || got.Unix() != time.Date(2026, 10, 31, 20, 30, 0, 0, ict).Unix() {
		t.Fatalf("got %v %v", got, err)
	}
	if _, err := parseSchedule("2026-10-31T03:05:00Z", now); err == nil {
		t.Fatal("5 minutes ahead must be rejected")
	}
	if _, err := parseSchedule("tomorrow", now); err == nil {
		t.Fatal("garbage must be rejected")
	}
}

func TestClassifyGraphError(t *testing.T) {
	cases := map[string]string{
		`{"error":{"message":"x","code":190}}`:                    "token_invalid",
		`{"error":{"message":"x","code":4}}`:                      "rate_limited",
		`{"error":{"message":"x","code":368}}`:                    "feature_blocked",
		`{"error":{"message":"x","code":200}}`:                    "permission_denied",
		`{"error":{"message":"x","code":100,"error_subcode":33}}`: "not_found",
		`{"error":{"message":"x","code":2}}`:                      "unavailable",
	}
	for body, want := range cases {
		_, err := parseGraph(400, []byte(body), false)
		var ae *apiError
		if !errors.As(err, &ae) || ae.Code != want {
			t.Errorf("%s: got %v, want %s", body, err, want)
		}
	}
	if out, err := parseGraph(200, []byte("true"), true); err != nil || out["success"] != true {
		t.Fatalf("bare true: %v %v", out, err)
	}
}

func TestToolSchemasAreValid(t *testing.T) {
	seen := map[string]bool{}
	for _, tl := range tools {
		if seen[tl.Name] || !strings.HasPrefix(tl.Name, "fbp_") || tl.Run == nil {
			t.Fatalf("bad tool %q", tl.Name)
		}
		seen[tl.Name] = true
		if _, err := json.Marshal(tl.Schema); err != nil {
			t.Fatal(err)
		}
	}
	if len(tools) != 20 {
		t.Fatalf("got %d tools", len(tools))
	}
}

// fakeRedis speaks the subset of the Upstash REST API the backend uses.
type fakeRedis struct {
	mu    sync.Mutex
	kv    map[string]string
	lists map[string][]string
}

func (f *fakeRedis) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer rtok" {
		w.WriteHeader(401)
		fmt.Fprint(w, `{"error":"unauthorized"}`)
		return
	}
	var args []string
	json.NewDecoder(r.Body).Decode(&args)
	f.mu.Lock()
	defer f.mu.Unlock()
	var res any
	switch strings.ToUpper(args[0]) {
	case "GET":
		if v, ok := f.kv[args[1]]; ok {
			res = v
		}
	case "SET":
		f.kv[args[1]] = args[2]
		res = "OK"
	case "DEL":
		for _, k := range args[1:] {
			delete(f.kv, k)
			delete(f.lists, k)
		}
		res = len(args) - 1
	case "RPUSH":
		f.lists[args[1]] = append(f.lists[args[1]], args[2])
		res = len(f.lists[args[1]])
	case "LTRIM":
		res = "OK"
	case "LRANGE":
		res = f.lists[args[1]]
	}
	json.NewEncoder(w).Encode(map[string]any{"result": res})
}

func hostedServer(t *testing.T, fg *fakeGraph, env map[string]string) (*server, *fakeRedis) {
	t.Helper()
	fr := &fakeRedis{kv: map[string]string{}, lists: map[string][]string{}}
	rs := httptest.NewServer(fr)
	gs := httptest.NewServer(fg)
	t.Cleanup(rs.Close)
	t.Cleanup(gs.Close)
	env["KV_REST_API_URL"], env["KV_REST_API_TOKEN"] = rs.URL, "rtok"
	rb := redisFromEnv(func(k string) string { return env[k] })
	if rb == nil {
		t.Fatal("redis backend not configured")
	}
	s := newServer(rb, gs.URL)
	s.remote = true
	return s, fr
}

func TestHostedRedisFlow(t *testing.T) {
	fg := &fakeGraph{}
	env := map[string]string{"FB_APP_ID": "1", "FB_APP_SECRET": "sec", "FB_USER_TOKEN": "SHORT-USER"}
	s, fr := hostedServer(t, fg, env)

	if _, err := call(t, s, "fbp_refresh_pages", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	// The long-lived token is reused while FB_USER_TOKEN stays the same...
	if c, _ := s.store.config(); c.UserToken != "LONG-USER" {
		t.Fatalf("want stored long-lived token, got %q", c.UserToken)
	}
	// ...and ignored once the env token is replaced.
	env["FB_USER_TOKEN"] = "NEW-USER"
	if c, _ := s.store.config(); c.UserToken != "NEW-USER" {
		t.Fatalf("want new env token, got %q", c.UserToken)
	}
	env["FB_USER_TOKEN"] = "SHORT-USER"

	if _, err := call(t, s, "fbp_create_post", map[string]any{"page_id": "777", "message": "x", "image_paths": []string{"/etc/hosts"}}); err == nil || err.Code != "invalid_input" {
		t.Fatalf("hosted server must refuse local paths, got %v", err)
	}
	if _, err := call(t, s, "fbp_create_post", map[string]any{"page_id": "777", "message": "x", "image_urls": []string{"ftp://x/a.jpg"}}); err == nil || err.Code != "invalid_input" {
		t.Fatalf("want invalid_input for ftp link, got %v", err)
	}
	out, err := call(t, s, "fbp_create_post", map[string]any{"page_id": "777", "message": "x", "image_urls": []string{"https://dieucaydep.vn/a.jpg"}})
	if err != nil || out["post_id"] != "777_999" {
		t.Fatalf("post: %v %v", out, err)
	}
	if len(fr.lists["fbp:history"]) != 1 {
		t.Fatalf("history not in redis: %v", fr.lists)
	}

	// A token error is stored, so another instance sees the stop too.
	call(t, s, "fbp_delete_post", map[string]any{"page_id": "777", "post_id": "bad"})
	other := newServer(s.store.b, s.gr.base)
	if other.trip() == nil {
		t.Fatal("write stop not shared through redis")
	}

	if _, err := call(t, s, "fbp_delete_stored_data", map[string]any{"confirm": "yes"}); err == nil {
		t.Fatal("deletion without exact confirm must fail")
	}
	if _, err := call(t, s, "fbp_delete_stored_data", map[string]any{"confirm": "DELETE"}); err != nil {
		t.Fatal(err)
	}
	if len(fr.kv) != 0 || len(fr.lists) != 0 {
		t.Fatalf("redis not empty: %v %v", fr.kv, fr.lists)
	}
}

func TestHTTPTransport(t *testing.T) {
	s := testServer(t, &fakeGraph{})
	h := httptest.NewServer(s.httpHandler("0123456789abcdef0123456789"))
	defer h.Close()

	post := func(auth, body string) *http.Response {
		req, _ := http.NewRequest("POST", h.URL+"/api/mcp", strings.NewReader(body))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if r := post("", `{"jsonrpc":"2.0","id":1,"method":"ping"}`); r.StatusCode != 401 {
		t.Fatalf("no auth: %d", r.StatusCode)
	}
	if r := post("Bearer wrong", `{"jsonrpc":"2.0","id":1,"method":"ping"}`); r.StatusCode != 401 {
		t.Fatalf("bad auth: %d", r.StatusCode)
	}
	const auth = "Bearer 0123456789abcdef0123456789"
	if r := post(auth, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); r.StatusCode != 202 {
		t.Fatalf("notification: %d", r.StatusCode)
	}
	r := post(auth, `{"jsonrpc":"2.0","id":7,"method":"tools/list"}`)
	var resp struct {
		ID     int `json:"id"`
		Result struct {
			Tools []any `json:"tools"`
		} `json:"result"`
	}
	json.NewDecoder(r.Body).Decode(&resp)
	if r.StatusCode != 200 || resp.ID != 7 || len(resp.Result.Tools) != len(tools) {
		t.Fatalf("tools/list: %d %+v", r.StatusCode, resp)
	}
	if g, _ := http.Get(h.URL + "/api/mcp"); g.StatusCode != 405 {
		t.Fatalf("GET: %d", g.StatusCode)
	}
	t.Setenv("FBP_OWNER_NAME", "Tre Shop")
	t.Setenv("FBP_CONTACT_EMAIL", "lienhe@example.com")
	for _, p := range []string{"/privacy", "/data-deletion"} {
		g, err := http.Get(h.URL + p) // public: no Authorization header
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(g.Body)
		if g.StatusCode != 200 || !strings.Contains(string(b), "lienhe@example.com") || !strings.Contains(string(b), "Tre Shop") {
			t.Fatalf("%s: %d %s", p, g.StatusCode, b[:min(len(b), 200)])
		}
	}
}

func TestConnectFlow(t *testing.T) {
	fg := &fakeGraph{}
	s, _ := hostedServer(t, fg, map[string]string{"FB_APP_ID": "1", "FB_APP_SECRET": "sec"})
	const secret = "0123456789abcdef0123456789"
	h := httptest.NewServer(s.httpHandler(secret))
	defer h.Close()
	jar, _ := cookiejar.New(nil)
	cl := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(path string) (*http.Response, string) {
		r, err := cl.Get(h.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(r.Body)
		return r, string(b)
	}
	post := func(path string, form url.Values, origin string) *http.Response {
		req, _ := http.NewRequest("POST", h.URL+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		r, err := cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	// Without an account there is nothing to refresh, and the hint points at /connect.
	if _, err := call(t, s, "fbp_refresh_pages", map[string]any{}); err == nil || err.Code != "no_accounts" {
		t.Fatalf("want no_accounts, got %v", err)
	}
	if _, body := get("/oauth/callback?code=GOODCODE&state=x"); !strings.Contains(body, "hết hạn") {
		t.Fatalf("callback without a session must be refused: %s", body)
	}
	if r := post("/connect/login", url.Values{"password": {secret}}, "https://evil.example"); r.StatusCode != 403 {
		t.Fatalf("cross-site login: %d", r.StatusCode)
	}
	// What browsers send: Sec-Fetch-Site decides, even with Origin: null.
	for site, want := range map[string]int{"same-origin": 303, "cross-site": 403, "same-site": 403} {
		req, _ := http.NewRequest("POST", h.URL+"/connect/login", strings.NewReader(url.Values{"password": {secret}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "null")
		req.Header.Set("Sec-Fetch-Site", site)
		r, err := (&http.Client{CheckRedirect: cl.CheckRedirect}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if r.StatusCode != want {
			t.Fatalf("Sec-Fetch-Site %s: got %d, want %d", site, r.StatusCode, want)
		}
	}
	if r := post("/connect/login", url.Values{"password": {"wrong"}}, ""); r.StatusCode != 401 {
		t.Fatalf("wrong password: %d", r.StatusCode)
	}
	if r := post("/connect/login", url.Values{"password": {secret}}, ""); r.StatusCode != 303 {
		t.Fatalf("login: %d", r.StatusCode)
	}

	r, _ := get("/connect/start?label=" + url.QueryEscape("Bất động sản"))
	loc, _ := url.Parse(r.Header.Get("Location"))
	if r.StatusCode != 302 || loc.Host != "www.facebook.com" || loc.Query().Get("client_id") != "1" || !strings.Contains(loc.Query().Get("scope"), "pages_manage_posts") {
		t.Fatalf("start: %d %s", r.StatusCode, loc)
	}
	state := loc.Query().Get("state")
	if _, body := get("/oauth/callback?code=GOODCODE&state=" + url.QueryEscape(state+"x")); !strings.Contains(body, "không hợp lệ") {
		t.Fatalf("tampered state accepted: %s", body)
	}
	_, body := get("/oauth/callback?code=GOODCODE&state=" + url.QueryEscape(state))
	if !strings.Contains(body, "Nhà Đất Thanh Hóa") || strings.Contains(body, "LONG-BDS") || strings.Contains(body, "PAGE-BDS") {
		t.Fatalf("callback page: %s", body)
	}

	accs, err := call(t, s, "fbp_list_accounts", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	a := accs["accounts"].([]any)[0].(map[string]any)
	if accs["count"] != float64(1) || a["label"] != "Bất động sản" || a["pages"] != float64(1) || a["source"] != "oauth" {
		t.Fatalf("accounts %v", accs)
	}
	pages, _ := call(t, s, "fbp_list_pages", map[string]any{"account_id": "43"})
	if p := pages["pages"].([]any)[0].(map[string]any); p["id"] != "888" || p["account_name"] != "Chủ Nhà Đất" {
		t.Fatalf("pages %v", pages)
	}
	if _, err := call(t, s, "fbp_create_post", map[string]any{"page_id": "888", "message": "x", "image_urls": []string{"https://a.vn/1.jpg"}}); err == nil || err.Code != "token_invalid" {
		// the fake only knows page 777's endpoints; reaching it with PAGE-BDS proves the right token is used
		t.Fatalf("want the fake's token_invalid for page 888, got %v", err)
	}
	fg.mu.Lock()
	usedTok := url.Values(fg.forms[len(fg.forms)-1]).Get("access_token")
	fg.mu.Unlock()
	if usedTok != "PAGE-BDS" {
		t.Fatalf("post to page 888 used token %q", usedTok)
	}
	s.setTrip(nil)

	if r := post("/connect/remove", url.Values{"account_id": {"43"}}, ""); r.StatusCode != 303 {
		t.Fatalf("remove: %d", r.StatusCode)
	}
	if accs, _ := call(t, s, "fbp_list_accounts", map[string]any{}); accs["count"] != float64(0) {
		t.Fatalf("account not removed: %v", accs)
	}
	if pf, _ := s.store.pages(); len(pf.Pages) != 0 {
		t.Fatalf("pages not removed: %v", pf.Pages)
	}
}
