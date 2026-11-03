package main

import (
	"context"
	"encoding/json"
	"net/url"
	"sort"
	"strings"
	"time"
)

// config holds the app credentials. user_token is optional: accounts are
// normally connected through Facebook Login at /connect, and a configured
// token is simply imported as one more account on fbp_refresh_pages.
type config struct {
	AppID        string `json:"app_id"`
	AppSecret    string `json:"app_secret"`
	UserToken    string `json:"user_token"`
	GraphVersion string `json:"graph_version"`
}

// account is one Facebook user whose Pages the server manages, with that
// user's long-lived token.
type account struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Label       string `json:"label,omitempty"`
	Token       string `json:"access_token"`
	Source      string `json:"source"` // oauth or config
	ConnectedAt string `json:"connected_at"`
}

type publicAccount struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Label       string `json:"label,omitempty"`
	Source      string `json:"source"`
	ConnectedAt string `json:"connected_at"`
	Pages       int    `json:"pages"`
}

// pageRecord is one managed Page with its Page access token.
type pageRecord struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Category    string   `json:"category,omitempty"`
	Tasks       []string `json:"tasks,omitempty"`
	AccountID   string   `json:"account_id,omitempty"`
	AccountName string   `json:"account_name,omitempty"`
	Token       string   `json:"access_token"`
}

type pagesFile struct {
	RefreshedAt string       `json:"refreshed_at"`
	Pages       []pageRecord `json:"pages"`
}

// publicPage is a pageRecord without its token, safe to return to the model.
type publicPage struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Category    string   `json:"category,omitempty"`
	Tasks       []string `json:"tasks,omitempty"`
	AccountID   string   `json:"account_id,omitempty"`
	AccountName string   `json:"account_name,omitempty"`
}

func (p pageRecord) public() publicPage {
	return publicPage{ID: p.ID, Name: p.Name, Category: p.Category, Tasks: p.Tasks, AccountID: p.AccountID, AccountName: p.AccountName}
}

func publicPages(ps []pageRecord, accountID string) []publicPage {
	out := make([]publicPage, 0, len(ps))
	for _, p := range ps {
		if accountID == "" || p.AccountID == accountID {
			out = append(out, p.public())
		}
	}
	return out
}

type store struct{ b backend }

// config is re-read on every call so edits apply without restarting.
func (st *store) config() (config, error) {
	c, err := st.b.loadConfig()
	c.AppID, c.AppSecret, c.UserToken = strings.TrimSpace(c.AppID), strings.TrimSpace(c.AppSecret), strings.TrimSpace(c.UserToken)
	return c, err
}

func (st *store) pages() (pagesFile, error) {
	var pf pagesFile
	b, err := st.b.loadPages()
	if err != nil {
		return pf, err
	}
	if b == nil {
		return pf, errf("no_pages", "no Page tokens stored yet; connect an account at /connect or run fbp_refresh_pages")
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
	return pageRecord{}, errf("unknown_page", "page %s is not among the %d managed Pages; check fbp_list_pages, or run fbp_refresh_pages if it was added recently", id, len(pf.Pages))
}

func (st *store) savePages(pf pagesFile) error {
	sort.Slice(pf.Pages, func(i, j int) bool { return pf.Pages[i].Name < pf.Pages[j].Name })
	b, err := json.MarshalIndent(pf, "", "  ")
	if err != nil {
		return err
	}
	return st.b.savePages(append(b, '\n'))
}

func (st *store) accounts() ([]account, error) {
	b, err := st.b.loadAccounts()
	if err != nil || b == nil {
		return nil, err
	}
	var as []account
	if err := json.Unmarshal(b, &as); err != nil {
		return nil, errf("storage_error", "stored accounts are corrupt: %v", err)
	}
	return as, nil
}

func (st *store) saveAccounts(as []account) error {
	sort.Slice(as, func(i, j int) bool { return as[i].Name < as[j].Name })
	b, err := json.MarshalIndent(as, "", "  ")
	if err != nil {
		return err
	}
	return st.b.saveAccounts(append(b, '\n'))
}

// upsertAccount adds a or replaces the account with the same Facebook ID,
// keeping an existing label when a has none.
func (st *store) upsertAccount(a account) error {
	as, err := st.accounts()
	if err != nil {
		return err
	}
	for i := range as {
		if as[i].ID == a.ID {
			if a.Label == "" {
				a.Label = as[i].Label
			}
			as[i] = a
			return st.saveAccounts(as)
		}
	}
	return st.saveAccounts(append(as, a))
}

// removeAccount drops the account and every Page stored through it.
func (st *store) removeAccount(id string) (account, int, error) {
	as, err := st.accounts()
	if err != nil {
		return account{}, 0, err
	}
	var gone account
	kept := as[:0]
	for _, a := range as {
		if a.ID == id {
			gone = a
			continue
		}
		kept = append(kept, a)
	}
	if gone.ID == "" {
		return account{}, 0, errf("unknown_account", "no connected account %s", id)
	}
	if err := st.saveAccounts(kept); err != nil {
		return account{}, 0, err
	}
	removed := 0
	if pf, err := st.pages(); err == nil {
		ps := pf.Pages[:0]
		for _, p := range pf.Pages {
			if p.AccountID == id {
				removed++
				continue
			}
			ps = append(ps, p)
		}
		pf.Pages = ps
		if err := st.savePages(pf); err != nil {
			return gone, removed, err
		}
	}
	return gone, removed, nil
}

func (st *store) publicAccounts() ([]publicAccount, error) {
	as, err := st.accounts()
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	if pf, err := st.pages(); err == nil {
		for _, p := range pf.Pages {
			counts[p.AccountID]++
		}
	}
	out := make([]publicAccount, 0, len(as))
	for _, a := range as {
		out = append(out, publicAccount{ID: a.ID, Name: a.Name, Label: a.Label, Source: a.Source, ConnectedAt: a.ConnectedAt, Pages: counts[a.ID]})
	}
	return out, nil
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

// tokenInfo asks /debug_token about a user token; it needs the app secret.
func (s *server) tokenInfo(ctx context.Context, c config, token string) map[string]any {
	if c.AppID == "" || c.AppSecret == "" {
		return nil
	}
	out, err := s.gr.call(ctx, "GET", "debug_token", c.AppID+"|"+c.AppSecret, url.Values{"input_token": {token}})
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

// longLived exchanges a user token for a long-lived one when the app secret
// is known; otherwise the token is returned unchanged.
func (s *server) longLived(ctx context.Context, c config, token string) (string, error) {
	if c.AppID == "" || c.AppSecret == "" {
		return token, nil
	}
	out, err := s.gr.call(ctx, "GET", "oauth/access_token", "", url.Values{
		"grant_type": {"fb_exchange_token"}, "client_id": {c.AppID},
		"client_secret": {c.AppSecret}, "fb_exchange_token": {token},
	})
	if err != nil {
		return "", err
	}
	if tok := str(out["access_token"]); tok != "" {
		return tok, nil
	}
	return token, nil
}

// connectAccount makes the token long-lived, identifies its user and stores
// the account. The caller refreshes its Pages.
func (s *server) connectAccount(ctx context.Context, c config, token, source, label string) (account, error) {
	tok, err := s.longLived(ctx, c, token)
	if err != nil {
		return account{}, err
	}
	me, err := s.gr.call(ctx, "GET", "me", tok, url.Values{"fields": {"id,name"}})
	if err != nil {
		return account{}, err
	}
	a := account{ID: str(me["id"]), Name: str(me["name"]), Label: strings.TrimSpace(label), Token: tok, Source: source, ConnectedAt: time.Now().Format(time.RFC3339)}
	if a.ID == "" {
		return account{}, errf("graph_error", "Facebook did not return the account id")
	}
	if err := s.store.upsertAccount(a); err != nil {
		return account{}, err
	}
	return a, nil
}

// accountPages lists every Page the account manages, with Page tokens.
// Page tokens derived from a long-lived user token do not expire.
func (s *server) accountPages(ctx context.Context, a account) ([]pageRecord, error) {
	var out []pageRecord
	params := url.Values{"fields": {"id,name,category,tasks,access_token"}, "limit": {"100"}}
	for i := 0; i < 20; i++ {
		resp, err := s.gr.call(ctx, "GET", "me/accounts", a.Token, params)
		if err != nil {
			return nil, err
		}
		pg := edgePage(resp)
		for _, d := range pg.Data {
			m, _ := d.(map[string]any)
			rec := pageRecord{ID: str(m["id"]), Name: str(m["name"]), Category: str(m["category"]), Token: str(m["access_token"]), AccountID: a.ID, AccountName: a.Name}
			if tasks, ok := m["tasks"].([]any); ok {
				for _, t := range tasks {
					rec.Tasks = append(rec.Tasks, str(t))
				}
			}
			if rec.ID != "" && rec.Token != "" {
				out = append(out, rec)
			}
		}
		if pg.NextCursor == "" {
			break
		}
		params.Set("after", pg.NextCursor)
	}
	return out, nil
}

// refreshPages imports a configured user token as an account, then reloads
// the Pages of every account (or only onlyID). An account whose token fails
// keeps its previously stored Pages and is reported under errors.
func (s *server) refreshPages(ctx context.Context, c config, onlyID string) (map[string]any, error) {
	result := map[string]any{}
	if c.UserToken != "" && onlyID == "" {
		a, err := s.connectAccount(ctx, c, c.UserToken, "config", "")
		if err != nil {
			result["config_token_error"] = err
		} else if a.Token != c.UserToken {
			if err := s.store.b.saveUserToken(a.Token); err != nil {
				return nil, err
			}
			result["config_token"] = "exchanged for a long-lived token and saved"
		}
	}
	as, err := s.store.accounts()
	if err != nil {
		return nil, err
	}
	if len(as) == 0 {
		return nil, errf("no_accounts", "no Facebook account connected: open %s/connect and press Kết nối Facebook", s.publicURL(nil))
	}

	old, _ := s.store.pages()
	byID := map[string]pageRecord{}
	refreshed := map[string]bool{}
	errs := map[string]any{}
	for _, a := range as {
		if onlyID != "" && a.ID != onlyID {
			continue
		}
		ps, err := s.accountPages(ctx, a)
		if err != nil {
			errs[a.ID+" ("+a.Name+")"] = err
			continue
		}
		refreshed[a.ID] = true
		for _, p := range ps {
			byID[p.ID] = p
		}
	}
	if onlyID != "" && !refreshed[onlyID] && len(errs) == 0 {
		return nil, errf("unknown_account", "no connected account %s", onlyID)
	}
	// Keep the Pages of accounts that were skipped or failed.
	known := map[string]bool{}
	for _, a := range as {
		known[a.ID] = true
	}
	for _, p := range old.Pages {
		if _, dup := byID[p.ID]; dup || refreshed[p.AccountID] || !known[p.AccountID] {
			continue
		}
		byID[p.ID] = p
	}
	pf := pagesFile{RefreshedAt: time.Now().Format(time.RFC3339)}
	for _, p := range byID {
		pf.Pages = append(pf.Pages, p)
	}
	if err := s.store.savePages(pf); err != nil {
		return nil, err
	}
	accs, _ := s.store.publicAccounts()
	result["accounts"] = accs
	result["pages"] = publicPages(pf.Pages, onlyID)
	if len(errs) > 0 {
		result["errors"] = errs
	}
	return result, nil
}
