package main

import (
	"context"
	"encoding/json"
	"net/url"
	"sort"
	"strings"
	"time"
)

// config is what the user writes by hand. Only user_token is required;
// app_id and app_secret enable the long-lived exchange, appsecret_proof and
// token debugging.
type config struct {
	AppID        string `json:"app_id"`
	AppSecret    string `json:"app_secret"`
	UserToken    string `json:"user_token"`
	GraphVersion string `json:"graph_version"`
}

// pageRecord is one managed Page with its Page access token, as stored in
// pages.json by fbp_refresh_pages.
type pageRecord struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Category string   `json:"category,omitempty"`
	Tasks    []string `json:"tasks,omitempty"`
	Token    string   `json:"access_token"`
}

type pagesFile struct {
	RefreshedAt string       `json:"refreshed_at"`
	UserID      string       `json:"user_id"`
	UserName    string       `json:"user_name"`
	Pages       []pageRecord `json:"pages"`
}

// publicPage is a pageRecord without its token, safe to return to the model.
type publicPage struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Category string   `json:"category,omitempty"`
	Tasks    []string `json:"tasks,omitempty"`
}

func (p pageRecord) public() publicPage {
	return publicPage{ID: p.ID, Name: p.Name, Category: p.Category, Tasks: p.Tasks}
}

type store struct{ b backend }

// config is re-read on every call so edits apply without restarting.
func (st *store) config() (config, error) {
	c, err := st.b.loadConfig()
	if err != nil {
		return c, err
	}
	c.AppID, c.AppSecret, c.UserToken = strings.TrimSpace(c.AppID), strings.TrimSpace(c.AppSecret), strings.TrimSpace(c.UserToken)
	if c.UserToken == "" {
		return c, errf("not_configured", "user_token is empty")
	}
	return c, nil
}

func (st *store) pages() (pagesFile, error) {
	var pf pagesFile
	b, err := st.b.loadPages()
	if err != nil {
		return pf, err
	}
	if b == nil {
		return pf, errf("no_pages", "no Page tokens stored yet; run fbp_refresh_pages")
	}
	if err := json.Unmarshal(b, &pf); err != nil {
		return pf, errf("no_pages", "stored Pages are corrupt (%v); run fbp_refresh_pages", err)
	}
	return pf, nil
}

func (st *store) page(id string) (pageRecord, error) {
	pf, err := st.pages()
	if err != nil {
		return pageRecord{}, err
	}
	for _, p := range pf.Pages {
		if p.ID == id {
			return p, nil
		}
	}
	names := make([]string, 0, len(pf.Pages))
	for _, p := range pf.Pages {
		names = append(names, p.ID+" ("+p.Name+")")
	}
	return pageRecord{}, errf("unknown_page", "page %s is not among the managed Pages: %s; run fbp_refresh_pages if it was added recently", id, strings.Join(names, ", "))
}

func (st *store) savePages(pf pagesFile) error {
	b, err := json.MarshalIndent(pf, "", "  ")
	if err != nil {
		return err
	}
	return st.b.savePages(append(b, '\n'))
}

// prepare applies the config to the Graph client and returns it.
func (s *server) prepare() (config, error) {
	c, err := s.store.config()
	if err != nil {
		return c, err
	}
	s.gr.mu.Lock()
	s.gr.secret = c.AppSecret
	if c.GraphVersion != "" {
		s.gr.version = c.GraphVersion
	} else {
		s.gr.version = defaultGraphVersion
	}
	s.gr.mu.Unlock()
	return c, nil
}

// tokenInfo asks /debug_token about the user token; it needs the app secret.
func (s *server) tokenInfo(ctx context.Context, c config) map[string]any {
	if c.AppID == "" || c.AppSecret == "" {
		return nil
	}
	out, err := s.gr.call(ctx, "GET", "debug_token", c.AppID+"|"+c.AppSecret, url.Values{"input_token": {c.UserToken}})
	if err != nil {
		return map[string]any{"error": err}
	}
	d, _ := out["data"].(map[string]any)
	info := map[string]any{"valid": d["is_valid"] == true, "type": d["type"], "scopes": d["scopes"]}
	if exp := toInt(d["expires_at"]); exp > 0 {
		info["expires_at"] = time.Unix(int64(exp), 0).Format(time.RFC3339)
	} else if d["is_valid"] == true {
		info["expires_at"] = "never"
	}
	return info
}

// refreshPages exchanges the user token for a long-lived one when possible,
// then stores every Page the user manages with its Page token. Page tokens
// derived from a long-lived user token do not expire.
func (s *server) refreshPages(ctx context.Context, c config) (map[string]any, error) {
	result := map[string]any{}
	if c.AppID != "" && c.AppSecret != "" {
		out, err := s.gr.call(ctx, "GET", "oauth/access_token", "", url.Values{
			"grant_type": {"fb_exchange_token"}, "client_id": {c.AppID},
			"client_secret": {c.AppSecret}, "fb_exchange_token": {c.UserToken},
		})
		if err != nil {
			return nil, err
		}
		if tok := str(out["access_token"]); tok != "" && tok != c.UserToken {
			if err := s.store.b.saveUserToken(tok); err != nil {
				return nil, err
			}
			c.UserToken = tok
			result["user_token"] = "exchanged for a long-lived token and saved"
		} else {
			result["user_token"] = "already long-lived"
		}
	} else {
		result["user_token"] = "kept as is; add app_id and app_secret to exchange it for a long-lived token"
	}

	me, err := s.gr.call(ctx, "GET", "me", c.UserToken, url.Values{"fields": {"id,name"}})
	if err != nil {
		return nil, err
	}
	pf := pagesFile{RefreshedAt: time.Now().Format(time.RFC3339), UserID: str(me["id"]), UserName: str(me["name"])}
	params := url.Values{"fields": {"id,name,category,tasks,access_token"}, "limit": {"100"}}
	for i := 0; i < 20; i++ {
		out, err := s.gr.call(ctx, "GET", "me/accounts", c.UserToken, params)
		if err != nil {
			return nil, err
		}
		pg := edgePage(out)
		for _, d := range pg.Data {
			m, _ := d.(map[string]any)
			rec := pageRecord{ID: str(m["id"]), Name: str(m["name"]), Category: str(m["category"]), Token: str(m["access_token"])}
			if tasks, ok := m["tasks"].([]any); ok {
				for _, t := range tasks {
					rec.Tasks = append(rec.Tasks, str(t))
				}
			}
			if rec.ID != "" && rec.Token != "" {
				pf.Pages = append(pf.Pages, rec)
			}
		}
		if pg.NextCursor == "" {
			break
		}
		params.Set("after", pg.NextCursor)
	}
	sort.Slice(pf.Pages, func(i, j int) bool { return pf.Pages[i].Name < pf.Pages[j].Name })
	if err := s.store.savePages(pf); err != nil {
		return nil, err
	}
	pub := make([]publicPage, 0, len(pf.Pages))
	for _, p := range pf.Pages {
		pub = append(pub, p.public())
	}
	result["account"] = map[string]any{"id": pf.UserID, "name": pf.UserName}
	result["pages"] = pub
	result["storage"] = s.store.b.locations()
	if len(pub) == 0 {
		result["hint"] = "no Pages returned: the token needs pages_show_list and the user must have a task (role) on the Page"
	}
	return result, nil
}
