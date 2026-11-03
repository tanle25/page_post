package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func objectSchema(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func intProp(desc string, min, max int) map[string]any {
	return map[string]any{"type": "integer", "minimum": min, "maximum": max, "description": desc}
}

func strList(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
}

var (
	pageIDProp    = strProp("numeric Page ID (from fbp_list_pages)")
	accountIDProp = strProp("optional: only this account (id from fbp_list_accounts)")
	postIDProp    = strProp("post ID as returned by Graph, usually <page_id>_<post_id>")
	cursorProp    = strProp("next_cursor from the previous page; omit for the first page")
	limitProp     = intProp("items per page, default 10", 0, 100)
	messageProp   = strProp("text; line breaks are kept")
)

// Fields read for a published post; counts come back as summaries only.
const postFields = "id,message,created_time,permalink_url,full_picture,status_type,is_published," +
	"shares,reactions.summary(total_count).limit(0),comments.summary(total_count).limit(0)"

// ict is Vietnam time, used to read schedule times without a zone. A fixed
// zone avoids depending on tzdata, which Windows lacks.
var ict = time.FixedZone("ICT", 7*3600)

var tools = []tool{
	// --- setup & pages
	{
		Name:        "fbp_status",
		Description: "Check the setup: app config, each connected Facebook account's token validity, expiry and scopes, and Page counts; also clears a write stop once every token is valid",
		Schema:      objectSchema(map[string]any{}),
		Run:         runStatus,
	},
	{
		Name:        "fbp_list_accounts",
		Description: "List the connected Facebook accounts (no tokens) with how many Pages each manages; new accounts are added in a browser at <server>/connect",
		Schema:      objectSchema(map[string]any{}),
		Run:         runListAccounts,
	},
	{
		Name:        "fbp_refresh_pages",
		Description: "Reload the Pages and Page tokens of every connected account (or one); run when a Page was added or a role changed",
		Schema:      objectSchema(map[string]any{"account_id": accountIDProp}),
		Run:         runRefreshPages,
	},
	{
		Name:        "fbp_list_pages",
		Description: "List the stored Pages (no tokens), each with the account it belongs to; optionally only one account's Pages",
		Schema:      objectSchema(map[string]any{"account_id": accountIDProp}),
		Run:         runListPages,
	},
	{
		Name:        "fbp_remove_account",
		Description: "Disconnect a Facebook account: forget its token and all of its Pages on this server (reconnect at <server>/connect)",
		Schema:      objectSchema(map[string]any{"account_id": strProp("account id from fbp_list_accounts")}, "account_id"),
		Run:         runRemoveAccount,
	},
	{
		Name:        "fbp_get_page",
		Description: "Read a Page's profile: name, category, about, followers, link, website and phone",
		Schema:      objectSchema(map[string]any{"page_id": pageIDProp}, "page_id"),
		Run:         runGetPage,
	},

	// --- reading
	{
		Name:        "fbp_get_posts",
		Description: "Read one page of a Page's published posts, newest first, with reaction, comment and share counts",
		Schema:      objectSchema(map[string]any{"page_id": pageIDProp, "limit": limitProp, "cursor": cursorProp}, "page_id"),
		Run:         runGetPosts,
	},
	{
		Name:        "fbp_get_scheduled_posts",
		Description: "List a Page's scheduled (not yet published) posts",
		Schema:      objectSchema(map[string]any{"page_id": pageIDProp, "limit": limitProp, "cursor": cursorProp}, "page_id"),
		Run:         runScheduledPosts,
	},
	{
		Name:        "fbp_get_post",
		Description: "Read a single Page post with its counts",
		Schema:      objectSchema(map[string]any{"page_id": pageIDProp, "post_id": postIDProp}, "page_id", "post_id"),
		Run:         runGetPost,
	},
	{
		Name:        "fbp_get_comments",
		Description: "Read one page of comments on a post, or replies to a comment, newest first",
		Schema: objectSchema(map[string]any{
			"page_id":   pageIDProp,
			"object_id": strProp("post ID, or comment ID to read its replies"),
			"limit":     limitProp,
			"cursor":    cursorProp,
		}, "page_id", "object_id"),
		Run: runGetComments,
	},
	{
		Name:        "fbp_page_insights",
		Description: "Read Page insights metrics; metric names follow Meta's Page Insights reference and change between Graph versions",
		Schema: objectSchema(map[string]any{
			"page_id": pageIDProp,
			"metrics": strList("metric names, default page_post_engagements, page_impressions_unique, page_follows"),
			"period":  map[string]any{"type": "string", "enum": []string{"day", "week", "days_28", "month", "lifetime", "total_over_range"}, "description": "default day"},
			"since":   strProp("optional start date YYYY-MM-DD"),
			"until":   strProp("optional end date YYYY-MM-DD"),
		}, "page_id"),
		Run: runPageInsights,
	},
	{
		Name:        "fbp_post_insights",
		Description: "Read insights for one Page post",
		Schema: objectSchema(map[string]any{
			"page_id": pageIDProp,
			"post_id": postIDProp,
			"metrics": strList("metric names, default post_impressions_unique, post_clicks, post_reactions_by_type_total"),
		}, "page_id", "post_id"),
		Run: runPostInsights,
	},
	{
		Name:        "fbp_history",
		Description: "Read this machine's log of Page writes (posts, edits, deletes, comment actions), newest first",
		Schema: objectSchema(map[string]any{
			"action":  map[string]any{"type": "string", "enum": []string{"create_post", "update_post", "delete_post", "reply_comment", "hide_comment", "delete_comment"}},
			"page_id": strProp("optional Page filter"),
			"status":  strProp("optional: ok, error (any failure) or a specific error code"),
			"limit":   intProp("max entries, default 50", 0, 1000),
		}),
		Run: runHistory,
	},

	// --- writes
	{
		Name:        "fbp_create_post",
		Description: "Publish a post with photos on a Page right away (or schedule it when asked) through the Graph API; 1-10 photos are required, as image_urls or image_paths",
		Schema: objectSchema(map[string]any{
			"page_id": pageIDProp,
			"message": messageProp,
			"image_urls": map[string]any{
				"type": "array", "minItems": 1, "maxItems": 10,
				"items":       map[string]any{"type": "string"},
				"description": "public http(s) image links in display order; Facebook downloads them. Use this or image_paths",
			},
			"image_paths": map[string]any{
				"type": "array", "minItems": 1, "maxItems": 10,
				"items":       map[string]any{"type": "string"},
				"description": "local image files (JPEG, PNG, GIF, BMP, TIFF; at most 10 MB each) in display order; only when the server runs locally",
			},
			"scheduled_time": strProp("optional, only when the user asks to schedule: publish later, at least 10 minutes from now; RFC3339 or \"2006-01-02 15:04\" in Vietnam time. Omit to publish immediately"),
		}, "page_id", "message"),
		Run: runCreatePost,
	},
	{
		Name:        "fbp_update_post",
		Description: "Replace the text of a Page post",
		Schema:      objectSchema(map[string]any{"page_id": pageIDProp, "post_id": postIDProp, "message": messageProp}, "page_id", "post_id", "message"),
		Run:         runUpdatePost,
	},
	{
		Name:        "fbp_delete_post",
		Description: "Delete a Page post, published or scheduled; this cannot be undone",
		Schema:      objectSchema(map[string]any{"page_id": pageIDProp, "post_id": postIDProp}, "page_id", "post_id"),
		Run:         runDeletePost,
	},
	{
		Name:        "fbp_reply_comment",
		Description: "Comment on a Page post, or reply to a comment, as the Page",
		Schema: objectSchema(map[string]any{
			"page_id":   pageIDProp,
			"object_id": strProp("post ID to comment on, or comment ID to reply to"),
			"message":   messageProp,
		}, "page_id", "object_id", "message"),
		Run: runReplyComment,
	},
	{
		Name:        "fbp_hide_comment",
		Description: "Hide or unhide a comment on a Page post; hidden comments stay visible to their author and friends",
		Schema: objectSchema(map[string]any{
			"page_id":    pageIDProp,
			"comment_id": strProp("comment ID"),
			"hidden":     map[string]any{"type": "boolean", "description": "true hides, false unhides; default true"},
		}, "page_id", "comment_id"),
		Run: runHideComment,
	},
	{
		Name:        "fbp_delete_comment",
		Description: "Delete a comment on a Page post; this cannot be undone",
		Schema:      objectSchema(map[string]any{"page_id": pageIDProp, "comment_id": strProp("comment ID")}, "page_id", "comment_id"),
		Run:         runDeleteComment,
	},
	{
		Name:        "fbp_delete_stored_data",
		Description: "Erase everything this server stores: the long-lived user token, all Page tokens, the write history and the write stop (honours a data deletion request). Cannot be undone; fbp_refresh_pages sets it up again",
		Schema:      objectSchema(map[string]any{"confirm": strProp(`must be exactly "DELETE"`)}, "confirm"),
		Run:         runDeleteStoredData,
	},
}

// read runs fn with the stored Page record.
func (s *server) read(ctx context.Context, pageID string, fn func(pageRecord) (any, error)) (any, error) {
	if _, err := s.prepare(); err != nil {
		return nil, err
	}
	p, err := s.store.page(pageID)
	if err != nil {
		return nil, err
	}
	out, err := fn(p)
	return out, s.noteError(err)
}

// write serialises writes, honours the write stop and logs every attempt.
func (s *server) write(ctx context.Context, entry historyEntry, fn func(pageRecord, *historyEntry) (any, error)) (any, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if t := s.trip(); t != nil {
		return nil, errf("write_stopped", "writing is stopped after %s (%s); fix it, then run fbp_status", t.Code, t.Message)
	}
	if _, err := s.prepare(); err != nil {
		s.hist.add(entry, err)
		return nil, err
	}
	p, err := s.store.page(entry.PageID)
	if err != nil {
		s.hist.add(entry, err)
		return nil, err
	}
	entry.PageName = p.Name
	out, err := fn(p, &entry)
	s.hist.add(entry, err)
	if err != nil {
		return nil, s.noteError(err)
	}
	return out, nil
}

// --- setup & pages

func runStatus(ctx context.Context, s *server, raw json.RawMessage) (any, error) {
	if err := decodeArgs(raw, &struct{}{}); err != nil {
		return nil, err
	}
	out := map[string]any{"storage": s.store.b.locations(), "hosted": s.remote}
	if s.remote {
		out["connect_url"] = s.publicURL(nil) + "/connect"
	}
	t := s.trip()
	if t != nil {
		out["write_stop"] = t
	}
	c, err := s.prepare()
	if err != nil {
		out["config_error"] = err
		out["setup_hint"] = "create a Meta app (type Business), set its app id and secret, then connect accounts at /connect (hosted) or put a user token in the config (local) and run fbp_refresh_pages"
		return out, nil
	}
	s.gr.mu.Lock()
	out["graph_version"] = s.gr.version
	s.gr.mu.Unlock()
	out["app_configured"] = c.AppID != "" && c.AppSecret != ""

	as, err := s.store.accounts()
	if err != nil {
		out["accounts_error"] = err
		return out, nil
	}
	counts := map[string]int{}
	if pf, err := s.store.pages(); err == nil {
		out["pages_refreshed_at"] = pf.RefreshedAt
		out["pages_total"] = len(pf.Pages)
		for _, p := range pf.Pages {
			counts[p.AccountID]++
		}
	}
	healthy := len(as) > 0
	list := make([]map[string]any, 0, len(as))
	for _, a := range as {
		row := map[string]any{"id": a.ID, "name": a.Name, "label": a.Label, "source": a.Source, "pages": counts[a.ID]}
		if _, err := s.gr.call(ctx, "GET", "me", a.Token, url.Values{"fields": {"id"}}); err != nil {
			row["token_error"] = err
			healthy = false
		} else if info := s.tokenInfo(ctx, c, a.Token); info != nil {
			row["token"] = info
		}
		list = append(list, row)
	}
	out["accounts"] = list
	if len(as) == 0 && c.UserToken != "" {
		out["hint"] = "a user token is configured but not imported yet; run fbp_refresh_pages"
	} else if len(as) == 0 {
		out["hint"] = "no Facebook account connected yet"
	}
	if t != nil && healthy {
		s.setTrip(nil)
		out["write_stop_cleared"] = t.Code
		delete(out, "write_stop")
	}
	return out, nil
}

func runListAccounts(ctx context.Context, s *server, raw json.RawMessage) (any, error) {
	if err := decodeArgs(raw, &struct{}{}); err != nil {
		return nil, err
	}
	accs, err := s.store.publicAccounts()
	if err != nil {
		return nil, err
	}
	out := map[string]any{"count": len(accs), "accounts": accs}
	if s.remote {
		out["connect_url"] = s.publicURL(nil) + "/connect"
	}
	return out, nil
}

func runRefreshPages(ctx context.Context, s *server, raw json.RawMessage) (any, error) {
	var in struct {
		AccountID string `json:"account_id"`
	}
	if err := decodeArgs(raw, &in); err != nil {
		return nil, err
	}
	c, err := s.prepare()
	if err != nil {
		return nil, err
	}
	out, err := s.refreshPages(ctx, c, in.AccountID)
	return out, s.noteError(err)
}

func runListPages(ctx context.Context, s *server, raw json.RawMessage) (any, error) {
	var in struct {
		AccountID string `json:"account_id"`
	}
	if err := decodeArgs(raw, &in); err != nil {
		return nil, err
	}
	pf, err := s.store.pages()
	if err != nil {
		return nil, err
	}
	pub := publicPages(pf.Pages, in.AccountID)
	return map[string]any{"refreshed_at": pf.RefreshedAt, "count": len(pub), "pages": pub}, nil
}

func runRemoveAccount(ctx context.Context, s *server, raw json.RawMessage) (any, error) {
	var in struct {
		AccountID string `json:"account_id"`
	}
	if err := decodeArgs(raw, &in); err != nil {
		return nil, err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	a, n, err := s.store.removeAccount(in.AccountID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"removed": a.Name, "account_id": a.ID, "pages_removed": n}, nil
}

func runGetPage(ctx context.Context, s *server, raw json.RawMessage) (any, error) {
	var in struct {
		PageID string `json:"page_id"`
	}
	if err := decodeArgs(raw, &in); err != nil {
		return nil, err
	}
	return s.read(ctx, in.PageID, func(p pageRecord) (any, error) {
		return s.gr.call(ctx, "GET", p.ID, p.Token, url.Values{
			"fields": {"id,name,category,about,description,fan_count,followers_count,link,website,phone,emails,verification_status"},
		})
	})
}

// --- reading

func pageParams(fields string, limit int, cursor string) url.Values {
	if limit <= 0 {
		limit = 10
	}
	v := url.Values{"fields": {fields}, "limit": {strconv.Itoa(limit)}}
	if cursor != "" {
		v.Set("after", cursor)
	}
	return v
}

// postView flattens Graph's summary objects into plain counts.
func postView(m map[string]any) map[string]any {
	for _, k := range []string{"reactions", "comments"} {
		if e, ok := m[k].(map[string]any); ok {
			if sum, ok := e["summary"].(map[string]any); ok {
				m[k+"_count"] = toInt(sum["total_count"])
			}
			delete(m, k)
		}
	}
	if sh, ok := m["shares"].(map[string]any); ok {
		m["shares_count"] = toInt(sh["count"])
		delete(m, "shares")
	} else {
		m["shares_count"] = 0
	}
	if t := toInt(m["scheduled_publish_time"]); t > 0 {
		m["scheduled_publish_time"] = time.Unix(int64(t), 0).In(ict).Format(time.RFC3339)
	}
	return m
}

func postsPage(out map[string]any) page {
	pg := edgePage(out)
	for _, d := range pg.Data {
		if m, ok := d.(map[string]any); ok {
			postView(m)
		}
	}
	return pg
}

type listInput struct {
	PageID string `json:"page_id"`
	Limit  int    `json:"limit"`
	Cursor string `json:"cursor"`
}

func runGetPosts(ctx context.Context, s *server, raw json.RawMessage) (any, error) {
	var in listInput
	if err := decodeArgs(raw, &in); err != nil {
		return nil, err
	}
	return s.read(ctx, in.PageID, func(p pageRecord) (any, error) {
		out, err := s.gr.call(ctx, "GET", p.ID+"/published_posts", p.Token, pageParams(postFields, in.Limit, in.Cursor))
		if err != nil {
			return nil, err
		}
		return postsPage(out), nil
	})
}

func runScheduledPosts(ctx context.Context, s *server, raw json.RawMessage) (any, error) {
	var in listInput
	if err := decodeArgs(raw, &in); err != nil {
		return nil, err
	}
	return s.read(ctx, in.PageID, func(p pageRecord) (any, error) {
		out, err := s.gr.call(ctx, "GET", p.ID+"/scheduled_posts", p.Token,
			pageParams("id,message,created_time,scheduled_publish_time,full_picture,is_published", in.Limit, in.Cursor))
		if err != nil {
			return nil, err
		}
		return postsPage(out), nil
	})
}

func runGetPost(ctx context.Context, s *server, raw json.RawMessage) (any, error) {
	var in struct {
		PageID string `json:"page_id"`
		PostID string `json:"post_id"`
	}
	if err := decodeArgs(raw, &in); err != nil {
		return nil, err
	}
	return s.read(ctx, in.PageID, func(p pageRecord) (any, error) {
		out, err := s.gr.call(ctx, "GET", in.PostID, p.Token, url.Values{"fields": {postFields + ",scheduled_publish_time,attachments{media_type,url,media,subattachments}"}})
		if err != nil {
			return nil, err
		}
		return postView(out), nil
	})
}

func runGetComments(ctx context.Context, s *server, raw json.RawMessage) (any, error) {
	var in struct {
		PageID   string `json:"page_id"`
		ObjectID string `json:"object_id"`
		Limit    int    `json:"limit"`
		Cursor   string `json:"cursor"`
	}
	if err := decodeArgs(raw, &in); err != nil {
		return nil, err
	}
	return s.read(ctx, in.PageID, func(p pageRecord) (any, error) {
		params := pageParams("id,message,from,created_time,like_count,comment_count,is_hidden,permalink_url,attachment", in.Limit, in.Cursor)
		params.Set("order", "reverse_chronological")
		params.Set("filter", "toplevel")
		out, err := s.gr.call(ctx, "GET", in.ObjectID+"/comments", p.Token, params)
		if err != nil {
			return nil, err
		}
		return edgePage(out), nil
	})
}

func runPageInsights(ctx context.Context, s *server, raw json.RawMessage) (any, error) {
	var in struct {
		PageID  string   `json:"page_id"`
		Metrics []string `json:"metrics"`
		Period  string   `json:"period"`
		Since   string   `json:"since"`
		Until   string   `json:"until"`
	}
	if err := decodeArgs(raw, &in); err != nil {
		return nil, err
	}
	if len(in.Metrics) == 0 {
		in.Metrics = []string{"page_post_engagements", "page_impressions_unique", "page_follows"}
	}
	if in.Period == "" {
		in.Period = "day"
	}
	params := url.Values{"metric": {strings.Join(in.Metrics, ",")}, "period": {in.Period}}
	for k, v := range map[string]string{"since": in.Since, "until": in.Until} {
		if v == "" {
			continue
		}
		if _, err := time.Parse("2006-01-02", v); err != nil {
			return nil, errf("invalid_input", "%s must be YYYY-MM-DD", k)
		}
		params.Set(k, v)
	}
	return s.read(ctx, in.PageID, func(p pageRecord) (any, error) {
		out, err := s.gr.call(ctx, "GET", p.ID+"/insights", p.Token, params)
		if err != nil {
			return nil, err
		}
		return edgePage(out), nil
	})
}

func runPostInsights(ctx context.Context, s *server, raw json.RawMessage) (any, error) {
	var in struct {
		PageID  string   `json:"page_id"`
		PostID  string   `json:"post_id"`
		Metrics []string `json:"metrics"`
	}
	if err := decodeArgs(raw, &in); err != nil {
		return nil, err
	}
	if len(in.Metrics) == 0 {
		in.Metrics = []string{"post_impressions_unique", "post_clicks", "post_reactions_by_type_total"}
	}
	return s.read(ctx, in.PageID, func(p pageRecord) (any, error) {
		out, err := s.gr.call(ctx, "GET", in.PostID+"/insights", p.Token, url.Values{"metric": {strings.Join(in.Metrics, ",")}})
		if err != nil {
			return nil, err
		}
		return edgePage(out), nil
	})
}

func runHistory(ctx context.Context, s *server, raw json.RawMessage) (any, error) {
	var in struct {
		Action string `json:"action"`
		PageID string `json:"page_id"`
		Status string `json:"status"`
		Limit  int    `json:"limit"`
	}
	if err := decodeArgs(raw, &in); err != nil {
		return nil, err
	}
	if in.Limit <= 0 {
		in.Limit = 50
	}
	entries, err := s.hist.query(in.Action, in.PageID, in.Status, in.Limit)
	if err != nil {
		return nil, err
	}
	return map[string]any{"count": len(entries), "entries": entries}, nil
}

// --- writes

// Graph accepts these photo formats; WebP and HEIC are rejected upstream.
var photoTypes = map[string]bool{"image/jpeg": true, "image/png": true, "image/gif": true, "image/bmp": true, "image/tiff": true}

const maxPhotoBytes = 10 << 20

func readImages(paths []string) ([]upload, error) {
	if len(paths) == 0 {
		return nil, errf("invalid_input", "image_urls or image_paths is required: Page posts must include at least one photo")
	}
	if len(paths) > 10 {
		return nil, errf("invalid_input", "at most 10 images per post")
	}
	ups := make([]upload, 0, len(paths))
	for _, p := range paths {
		p = expandHome(p)
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, errf("invalid_input", "reading image: %v", err)
		}
		if len(data) > maxPhotoBytes {
			return nil, errf("invalid_input", "%s is %.1f MB; Facebook accepts at most 10 MB per photo", p, float64(len(data))/(1<<20))
		}
		ct := http.DetectContentType(data)
		if ext := strings.ToLower(filepath.Ext(p)); ct == "application/octet-stream" && (ext == ".tif" || ext == ".tiff") {
			ct = "image/tiff"
		}
		if !photoTypes[ct] {
			return nil, errf("invalid_input", "%s is %s; use JPEG, PNG, GIF, BMP or TIFF", p, ct)
		}
		ups = append(ups, upload{Field: "source", Name: filepath.Base(p), Type: ct, Data: data})
	}
	return ups, nil
}

// parseSchedule reads RFC3339, or a zone-less "2006-01-02 15:04" as Vietnam
// time. Facebook requires at least 10 minutes of lead time.
func parseSchedule(v string, now time.Time) (time.Time, error) {
	v = strings.TrimSpace(v)
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		t, err = time.ParseInLocation("2006-01-02 15:04", v, ict)
	}
	if err != nil {
		return time.Time{}, errf("invalid_input", "scheduled_time %q: use RFC3339 or \"2006-01-02 15:04\" (Vietnam time)", v)
	}
	if t.Before(now.Add(10 * time.Minute)) {
		return time.Time{}, errf("invalid_input", "scheduled_time must be at least 10 minutes from now (now is %s)", now.In(ict).Format("2006-01-02 15:04"))
	}
	return t, nil
}

// photoSource is one photo to upload: a local file or a public link.
type photoSource struct {
	file *upload
	url  string
}

func (p photoSource) label() string {
	if p.file != nil {
		return p.file.Name
	}
	return p.url
}

func checkImageURLs(urls []string) ([]photoSource, error) {
	if len(urls) > 10 {
		return nil, errf("invalid_input", "at most 10 images per post")
	}
	out := make([]photoSource, 0, len(urls))
	for _, raw := range urls {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return nil, errf("invalid_input", "image_urls: %q is not an http(s) link", raw)
		}
		out = append(out, photoSource{url: u.String()})
	}
	return out, nil
}

// photoSources validates the images before Facebook is touched.
func (s *server) photoSources(paths, urls []string) ([]photoSource, error) {
	switch {
	case len(paths) > 0 && len(urls) > 0:
		return nil, errf("invalid_input", "use image_urls or image_paths, not both")
	case len(urls) > 0:
		return checkImageURLs(urls)
	case len(paths) > 0 && s.remote:
		return nil, errf("invalid_input", "image_paths cannot be read by the hosted server; pass public links in image_urls")
	}
	ups, err := readImages(paths)
	if err != nil {
		return nil, err
	}
	out := make([]photoSource, len(ups))
	for i := range ups {
		out[i] = photoSource{file: &ups[i]}
	}
	return out, nil
}

func runCreatePost(ctx context.Context, s *server, raw json.RawMessage) (any, error) {
	var in struct {
		PageID        string   `json:"page_id"`
		Message       string   `json:"message"`
		ImagePaths    []string `json:"image_paths"`
		ImageURLs     []string `json:"image_urls"`
		ScheduledTime string   `json:"scheduled_time"`
	}
	if err := decodeArgs(raw, &in); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Message) == "" {
		return nil, errf("invalid_input", "message is empty")
	}
	// Images and the schedule are checked before Facebook is touched.
	photos, err := s.photoSources(in.ImagePaths, in.ImageURLs)
	if err != nil {
		return nil, err
	}
	var when time.Time
	if in.ScheduledTime != "" {
		if when, err = parseSchedule(in.ScheduledTime, time.Now()); err != nil {
			return nil, err
		}
	}
	entry := historyEntry{Action: "create_post", PageID: in.PageID, Message: in.Message, Images: append(in.ImagePaths, in.ImageURLs...)}
	if !when.IsZero() {
		entry.ScheduledFor = when.In(ict).Format(time.RFC3339)
	}
	return s.write(ctx, entry, func(p pageRecord, e *historyEntry) (any, error) {
		// Upload each photo unpublished, then attach them all to one feed post.
		for _, ph := range photos {
			params := url.Values{"published": {"false"}}
			if !when.IsZero() {
				params.Set("temporary", "true") // required for photos of a scheduled post
			}
			var files []upload
			if ph.file != nil {
				files = append(files, *ph.file)
			} else {
				params.Set("url", ph.url)
			}
			out, err := s.gr.call(ctx, "POST", p.ID+"/photos", p.Token, params, files...)
			if err != nil {
				return nil, err
			}
			id := str(out["id"])
			if id == "" {
				return nil, errf("upload_failed", "no photo id returned for %s", ph.label())
			}
			e.PhotoIDs = append(e.PhotoIDs, id)
		}
		params := url.Values{"message": {in.Message}}
		for i, id := range e.PhotoIDs {
			b, _ := json.Marshal(map[string]string{"media_fbid": id})
			params.Set(fmt.Sprintf("attached_media[%d]", i), string(b))
		}
		if !when.IsZero() {
			params.Set("published", "false")
			params.Set("scheduled_publish_time", strconv.FormatInt(when.Unix(), 10))
		}
		out, err := s.gr.call(ctx, "POST", p.ID+"/feed", p.Token, params)
		if err != nil {
			return nil, err
		}
		e.ResultID = str(out["id"])
		if e.ResultID == "" {
			return nil, errf("write_unknown", "Facebook returned no post id; check the Page before retrying")
		}
		res := map[string]any{"post_id": e.ResultID, "page_id": p.ID, "page_name": p.Name, "photo_ids": e.PhotoIDs}
		if !when.IsZero() {
			res["scheduled_for"] = e.ScheduledFor
			return res, nil
		}
		if info, err := s.gr.call(ctx, "GET", e.ResultID, p.Token, url.Values{"fields": {"permalink_url"}}); err == nil {
			e.Permalink = str(info["permalink_url"])
			res["permalink"] = e.Permalink
		}
		return res, nil
	})
}

func runUpdatePost(ctx context.Context, s *server, raw json.RawMessage) (any, error) {
	var in struct {
		PageID  string `json:"page_id"`
		PostID  string `json:"post_id"`
		Message string `json:"message"`
	}
	if err := decodeArgs(raw, &in); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Message) == "" {
		return nil, errf("invalid_input", "message is empty")
	}
	entry := historyEntry{Action: "update_post", PageID: in.PageID, TargetID: in.PostID, Message: in.Message}
	return s.write(ctx, entry, func(p pageRecord, e *historyEntry) (any, error) {
		out, err := s.gr.call(ctx, "POST", in.PostID, p.Token, url.Values{"message": {in.Message}})
		if err != nil {
			return nil, err
		}
		return map[string]any{"post_id": in.PostID, "success": out["success"] == true}, nil
	})
}

func runDeletePost(ctx context.Context, s *server, raw json.RawMessage) (any, error) {
	var in struct {
		PageID string `json:"page_id"`
		PostID string `json:"post_id"`
	}
	if err := decodeArgs(raw, &in); err != nil {
		return nil, err
	}
	entry := historyEntry{Action: "delete_post", PageID: in.PageID, TargetID: in.PostID}
	return s.write(ctx, entry, func(p pageRecord, e *historyEntry) (any, error) {
		out, err := s.gr.call(ctx, "DELETE", in.PostID, p.Token, nil)
		if err != nil {
			return nil, err
		}
		return map[string]any{"post_id": in.PostID, "deleted": out["success"] == true}, nil
	})
}

func runReplyComment(ctx context.Context, s *server, raw json.RawMessage) (any, error) {
	var in struct {
		PageID   string `json:"page_id"`
		ObjectID string `json:"object_id"`
		Message  string `json:"message"`
	}
	if err := decodeArgs(raw, &in); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Message) == "" {
		return nil, errf("invalid_input", "message is empty")
	}
	entry := historyEntry{Action: "reply_comment", PageID: in.PageID, TargetID: in.ObjectID, Message: in.Message}
	return s.write(ctx, entry, func(p pageRecord, e *historyEntry) (any, error) {
		out, err := s.gr.call(ctx, "POST", in.ObjectID+"/comments", p.Token, url.Values{"message": {in.Message}})
		if err != nil {
			return nil, err
		}
		e.ResultID = str(out["id"])
		return map[string]any{"comment_id": e.ResultID, "on": in.ObjectID}, nil
	})
}

func runHideComment(ctx context.Context, s *server, raw json.RawMessage) (any, error) {
	var in struct {
		PageID    string `json:"page_id"`
		CommentID string `json:"comment_id"`
		Hidden    *bool  `json:"hidden"`
	}
	if err := decodeArgs(raw, &in); err != nil {
		return nil, err
	}
	hide := in.Hidden == nil || *in.Hidden
	entry := historyEntry{Action: "hide_comment", PageID: in.PageID, TargetID: in.CommentID, Message: strconv.FormatBool(hide)}
	return s.write(ctx, entry, func(p pageRecord, e *historyEntry) (any, error) {
		out, err := s.gr.call(ctx, "POST", in.CommentID, p.Token, url.Values{"is_hidden": {strconv.FormatBool(hide)}})
		if err != nil {
			return nil, err
		}
		return map[string]any{"comment_id": in.CommentID, "hidden": hide, "success": out["success"] == true}, nil
	})
}

func runDeleteStoredData(ctx context.Context, s *server, raw json.RawMessage) (any, error) {
	var in struct {
		Confirm string `json:"confirm"`
	}
	if err := decodeArgs(raw, &in); err != nil {
		return nil, err
	}
	if in.Confirm != "DELETE" {
		return nil, errf("invalid_input", `confirm must be exactly "DELETE"`)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.store.b.deleteAll(); err != nil {
		return nil, err
	}
	return map[string]any{"deleted": true, "storage": s.store.b.locations()}, nil
}

func runDeleteComment(ctx context.Context, s *server, raw json.RawMessage) (any, error) {
	var in struct {
		PageID    string `json:"page_id"`
		CommentID string `json:"comment_id"`
	}
	if err := decodeArgs(raw, &in); err != nil {
		return nil, err
	}
	entry := historyEntry{Action: "delete_comment", PageID: in.PageID, TargetID: in.CommentID}
	return s.write(ctx, entry, func(p pageRecord, e *historyEntry) (any, error) {
		out, err := s.gr.call(ctx, "DELETE", in.CommentID, p.Token, nil)
		if err != nil {
			return nil, err
		}
		return map[string]any{"comment_id": in.CommentID, "deleted": out["success"] == true}, nil
	})
}
