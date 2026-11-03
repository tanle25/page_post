package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// backend is where the server keeps its state: the config, the Page tokens,
// the write history and the write stop. Locally that is ~/.facebook-go; on
// Vercel, whose filesystem does not persist, it is Upstash Redis.
type backend interface {
	loadConfig() (config, error)
	saveUserToken(token string) error
	loadPages() ([]byte, error) // nil when nothing is stored yet
	savePages(b []byte) error
	loadAccounts() ([]byte, error) // nil when no account is connected
	saveAccounts(b []byte) error
	appendHistory(line []byte) error
	historyLines() ([][]byte, error) // oldest first
	loadTrip() ([]byte, error)
	saveTrip(b []byte) error // nil clears the stop
	locations() map[string]string
	deleteAll() error // accounts, tokens, Pages, history and the write stop
}

// --- local files

type fileBackend struct {
	configPath, pagesPath, accountsPath, historyPath, tripPath string
	mu                                                         sync.Mutex
}

func newFileBackend(dir, configPath string) *fileBackend {
	return &fileBackend{
		configPath:   expandHome(configPath),
		pagesPath:    filepath.Join(dir, "pages.json"),
		accountsPath: filepath.Join(dir, "page-accounts.json"),
		historyPath:  filepath.Join(dir, "page-history.jsonl"),
		tripPath:     filepath.Join(dir, "page-write-stop.json"),
	}
}

func (f *fileBackend) locations() map[string]string {
	return map[string]string{"kind": "files", "config": f.configPath, "pages": f.pagesPath, "history": f.historyPath}
}

func (f *fileBackend) loadConfig() (config, error) {
	var c config
	b, err := os.ReadFile(f.configPath)
	if errors.Is(err, os.ErrNotExist) {
		return c, errf("not_configured", "create %s with {\"app_id\":\"…\",\"app_secret\":\"…\",\"user_token\":\"…\"}", f.configPath)
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, errf("not_configured", "%s is not valid JSON: %v", f.configPath, err)
	}
	return c, nil
}

// saveUserToken replaces user_token in the config, keeping the other fields.
func (f *fileBackend) saveUserToken(token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, err := os.ReadFile(f.configPath)
	if err != nil {
		return err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	m["user_token"] = token
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(f.configPath, append(out, '\n'))
}

func (f *fileBackend) loadPages() ([]byte, error)    { return readOptional(f.pagesPath) }
func (f *fileBackend) savePages(b []byte) error      { return writePrivate(f.pagesPath, b) }
func (f *fileBackend) loadTrip() ([]byte, error)     { return readOptional(f.tripPath) }
func (f *fileBackend) loadAccounts() ([]byte, error) { return readOptional(f.accountsPath) }
func (f *fileBackend) saveAccounts(b []byte) error   { return writePrivate(f.accountsPath, b) }

func (f *fileBackend) saveTrip(b []byte) error {
	if b == nil {
		if err := os.Remove(f.tripPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	return writePrivate(f.tripPath, b)
}

func (f *fileBackend) appendHistory(line []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(f.historyPath), 0o700); err != nil {
		return err
	}
	fh, err := os.OpenFile(f.historyPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer fh.Close()
	_, err = fh.Write(append(line, '\n'))
	return err
}

func (f *fileBackend) historyLines() ([][]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, err := readOptional(f.historyPath)
	if err != nil || b == nil {
		return nil, err
	}
	var lines [][]byte
	for _, l := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(l)) > 0 {
			lines = append(lines, l)
		}
	}
	return lines, nil
}

func (f *fileBackend) deleteAll() error {
	f.mu.Lock()
	for _, p := range []string{f.pagesPath, f.accountsPath, f.historyPath, f.tripPath} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			f.mu.Unlock()
			return err
		}
	}
	f.mu.Unlock()
	if _, err := os.Stat(f.configPath); err == nil {
		return f.saveUserToken("")
	}
	return nil
}

func readOptional(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

func writePrivate(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// --- Upstash Redis (REST API)

// historyCap bounds the Redis history list; older entries are dropped.
const historyCap = 10000

// redisBackend reads the config from environment variables (FB_APP_ID,
// FB_APP_SECRET, FB_USER_TOKEN, FB_GRAPH_VERSION). Environment variables
// cannot be changed at runtime, so the long-lived user token from
// fbp_refresh_pages is kept in Redis, tagged with a hash of the env token it
// came from: replacing FB_USER_TOKEN makes the stored one stale.
type redisBackend struct {
	url, token, prefix string
	env                func(string) string
	hc                 *http.Client
}

// redisFromEnv accepts both the Upstash names and the Vercel KV names that
// the Vercel Marketplace integration sets.
func redisFromEnv(env func(string) string) *redisBackend {
	url, token := env("UPSTASH_REDIS_REST_URL"), env("UPSTASH_REDIS_REST_TOKEN")
	if url == "" || token == "" {
		url, token = env("KV_REST_API_URL"), env("KV_REST_API_TOKEN")
	}
	if url == "" || token == "" {
		return nil
	}
	prefix := env("FBP_REDIS_PREFIX")
	if prefix == "" {
		prefix = "fbp:"
	}
	return &redisBackend{url: strings.TrimRight(url, "/"), token: token, prefix: prefix, env: env, hc: &http.Client{Timeout: 15 * time.Second}}
}

func (r *redisBackend) locations() map[string]string {
	return map[string]string{"kind": "upstash_redis", "key_prefix": r.prefix, "config": "environment variables FB_APP_ID, FB_APP_SECRET (FB_USER_TOKEN optional)"}
}

// cmd runs one Redis command; the result is nil for a missing key.
func (r *redisBackend) cmd(args ...string) (any, error) {
	body, _ := json.Marshal(args)
	req, err := http.NewRequest(http.MethodPost, r.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	resp, err := r.hc.Do(req)
	if err != nil {
		return nil, &apiError{Code: "storage_error", Message: err.Error(), Retryable: true}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	var out struct {
		Result any    `json:"result"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, &apiError{Code: "storage_error", Message: fmt.Sprintf("Redis HTTP %d", resp.StatusCode)}
	}
	if out.Error != "" {
		return nil, &apiError{Code: "storage_error", Message: "Redis: " + out.Error}
	}
	return out.Result, nil
}

func (r *redisBackend) get(key string) ([]byte, error) {
	v, err := r.cmd("GET", r.prefix+key)
	if err != nil || v == nil {
		return nil, err
	}
	s, _ := v.(string)
	return []byte(s), nil
}

func (r *redisBackend) set(key string, b []byte) error {
	_, err := r.cmd("SET", r.prefix+key, string(b))
	return err
}

func tokenTag(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:8])
}

type storedToken struct {
	Source string `json:"source"` // tokenTag of the FB_USER_TOKEN it was exchanged from
	Token  string `json:"token"`
}

func (r *redisBackend) loadConfig() (config, error) {
	c := config{AppID: r.env("FB_APP_ID"), AppSecret: r.env("FB_APP_SECRET"), UserToken: r.env("FB_USER_TOKEN"), GraphVersion: r.env("FB_GRAPH_VERSION")}
	if strings.TrimSpace(c.AppID) == "" || strings.TrimSpace(c.AppSecret) == "" {
		return c, errf("not_configured", "set FB_APP_ID and FB_APP_SECRET in the Vercel project's environment variables, then redeploy")
	}
	if strings.TrimSpace(c.UserToken) == "" {
		return c, nil // accounts connect through /connect instead
	}
	b, err := r.get("user_token")
	if err != nil {
		return c, err
	}
	var st storedToken
	if b != nil && json.Unmarshal(b, &st) == nil && st.Source == tokenTag(strings.TrimSpace(c.UserToken)) && st.Token != "" {
		c.UserToken = st.Token
	}
	return c, nil
}

func (r *redisBackend) saveUserToken(token string) error {
	b, _ := json.Marshal(storedToken{Source: tokenTag(strings.TrimSpace(r.env("FB_USER_TOKEN"))), Token: token})
	return r.set("user_token", b)
}

func (r *redisBackend) loadPages() ([]byte, error)    { return r.get("pages") }
func (r *redisBackend) savePages(b []byte) error      { return r.set("pages", b) }
func (r *redisBackend) loadTrip() ([]byte, error)     { return r.get("write_stop") }
func (r *redisBackend) loadAccounts() ([]byte, error) { return r.get("accounts") }
func (r *redisBackend) saveAccounts(b []byte) error   { return r.set("accounts", b) }

func (r *redisBackend) saveTrip(b []byte) error {
	if b == nil {
		_, err := r.cmd("DEL", r.prefix+"write_stop")
		return err
	}
	return r.set("write_stop", b)
}

func (r *redisBackend) appendHistory(line []byte) error {
	if _, err := r.cmd("RPUSH", r.prefix+"history", string(line)); err != nil {
		return err
	}
	_, err := r.cmd("LTRIM", r.prefix+"history", fmt.Sprint(-historyCap), "-1")
	return err
}

func (r *redisBackend) deleteAll() error {
	_, err := r.cmd("DEL", r.prefix+"user_token", r.prefix+"accounts", r.prefix+"pages", r.prefix+"history", r.prefix+"write_stop")
	return err
}

func (r *redisBackend) historyLines() ([][]byte, error) {
	v, err := r.cmd("LRANGE", r.prefix+"history", "0", "-1")
	if err != nil {
		return nil, err
	}
	items, _ := v.([]any)
	lines := make([][]byte, 0, len(items))
	for _, it := range items {
		if s, ok := it.(string); ok {
			lines = append(lines, []byte(s))
		}
	}
	return lines, nil
}
