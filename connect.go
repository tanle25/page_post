package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// /connect lets the owner add Facebook accounts through Facebook Login from
// any browser: sign in with FBP_AUTH_TOKEN, press Kết nối Facebook, approve
// the permissions, and the server keeps a long-lived token for the account
// and a Page token for each of its Pages. Tokens never leave the server.

const (
	sessionCookie = "fbp_session"
	sessionTTL    = 12 * time.Hour
	stateTTL      = 10 * time.Minute
)

// loginScopes are requested when FB_LOGIN_CONFIG_ID is not set. Apps of type
// Business use Facebook Login for Business, whose configuration (with the
// same permissions) is referenced by config_id instead.
var loginScopes = []string{"pages_show_list", "pages_read_engagement", "pages_manage_posts", "pages_manage_engagement", "read_insights"}

// publicURL is the server's own origin, used for the OAuth redirect. It never
// comes from the Host header on Vercel, where that could be spoofed.
func (s *server) publicURL(r *http.Request) string {
	if u := strings.TrimRight(os.Getenv("FBP_PUBLIC_URL"), "/"); u != "" {
		return u
	}
	if h := os.Getenv("VERCEL_PROJECT_PRODUCTION_URL"); h != "" {
		return "https://" + h
	}
	if r != nil {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		return scheme + "://" + r.Host
	}
	return "https://<your-deployment>"
}

// webKey signs the session cookie and the OAuth state; it is derived from
// the MCP secret so rotating FBP_AUTH_TOKEN signs everyone out.
type webKey []byte

func newWebKey(authToken string) webKey {
	m := hmac.New(sha256.New, []byte("fbp-web"))
	m.Write([]byte(authToken))
	return m.Sum(nil)
}

func (k webKey) sign(msg string) string {
	m := hmac.New(sha256.New, k)
	m.Write([]byte(msg))
	return hex.EncodeToString(m.Sum(nil))
}

// signed returns "payload.sig" for a payload that expires at exp.
func (k webKey) signed(kind, payload string, exp time.Time) string {
	body := base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + strconv.FormatInt(exp.Unix(), 10)
	return body + "." + k.sign(kind+"|"+body)
}

func (k webKey) verify(kind, token string) (string, bool) {
	i := strings.LastIndexByte(token, '.')
	if i < 0 {
		return "", false
	}
	body, sig := token[:i], token[i+1:]
	if subtle.ConstantTimeCompare([]byte(sig), []byte(k.sign(kind+"|"+body))) != 1 {
		return "", false
	}
	j := strings.LastIndexByte(body, '.')
	if j < 0 {
		return "", false
	}
	exp, err := strconv.ParseInt(body[j+1:], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(body[:j])
	if err != nil {
		return "", false
	}
	return string(payload), true
}

type connectWeb struct {
	s    *server
	key  webKey
	want [32]byte // sha256 of FBP_AUTH_TOKEN
}

func (s *server) connectHandlers(mux *http.ServeMux, authToken string) {
	cw := &connectWeb{s: s, key: newWebKey(authToken), want: sha256.Sum256([]byte(authToken))}
	mux.HandleFunc("/connect", cw.home)
	mux.HandleFunc("/connect/login", cw.login)
	mux.HandleFunc("/connect/logout", cw.logout)
	mux.HandleFunc("/connect/start", cw.start)
	mux.HandleFunc("/connect/remove", cw.remove)
	mux.HandleFunc("/oauth/callback", cw.callback)
}

func (cw *connectWeb) signedIn(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	_, ok := cw.key.verify("session", c.Value)
	return ok
}

// sameOrigin rejects cross-site form posts. Browsers send Sec-Fetch-Site;
// Origin is the fallback for older ones and may be "null" under strict
// referrer policies, so it only counts when it names a real origin.
func (cw *connectWeb) sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site == "same-origin" || site == "none"
	}
	o := r.Header.Get("Origin")
	return o == "" || o == "null" || o == cw.s.publicURL(r)
}

type connectView struct {
	SignedIn    bool
	Error       string
	Notice      string
	Accounts    []publicAccount
	Pages       []publicPage
	RedirectURI string
	ConfigID    bool
	Connected   *account
}

func (cw *connectWeb) render(w http.ResponseWriter, status int, v connectView) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.WriteHeader(status)
	if err := connectTmpl.Execute(w, v); err != nil {
		log.Printf("connect page: %v", err)
	}
}

func (cw *connectWeb) home(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !cw.signedIn(r) {
		cw.render(w, http.StatusOK, connectView{})
		return
	}
	v := connectView{SignedIn: true, RedirectURI: cw.s.publicURL(r) + "/oauth/callback", ConfigID: os.Getenv("FB_LOGIN_CONFIG_ID") != "", Notice: r.URL.Query().Get("notice")}
	accs, err := cw.s.store.publicAccounts()
	if err != nil {
		v.Error = err.Error()
	}
	v.Accounts = accs
	cw.render(w, http.StatusOK, v)
}

func (cw *connectWeb) login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !cw.sameOrigin(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	got := sha256.Sum256([]byte(strings.TrimSpace(r.PostFormValue("password"))))
	if subtle.ConstantTimeCompare(got[:], cw.want[:]) != 1 {
		time.Sleep(time.Second) // slow down guessing
		cw.render(w, http.StatusUnauthorized, connectView{Error: "Sai mật khẩu. Dùng giá trị FBP_AUTH_TOKEN."})
		return
	}
	exp := time.Now().Add(sessionTTL)
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: cw.key.signed("session", "owner", exp), Path: "/", Expires: exp,
		HttpOnly: true, Secure: r.TLS != nil || os.Getenv("VERCEL") != "", SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/connect", http.StatusSeeOther)
}

func (cw *connectWeb) logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !cw.sameOrigin(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/connect", http.StatusSeeOther)
}

type oauthState struct {
	Label string `json:"l"`
	Nonce string `json:"n"`
}

// start sends the browser to Facebook's login dialog.
func (cw *connectWeb) start(w http.ResponseWriter, r *http.Request) {
	if !cw.signedIn(r) {
		http.Redirect(w, r, "/connect", http.StatusSeeOther)
		return
	}
	c, err := cw.s.prepare()
	if err != nil {
		cw.render(w, http.StatusOK, connectView{SignedIn: true, Error: err.Error()})
		return
	}
	nonce := make([]byte, 12)
	rand.Read(nonce)
	label := strings.TrimSpace(r.URL.Query().Get("label"))
	if len([]rune(label)) > 60 {
		label = string([]rune(label)[:60])
	}
	st, _ := json.Marshal(oauthState{Label: label, Nonce: hex.EncodeToString(nonce)})

	cw.s.gr.mu.Lock()
	version := cw.s.gr.version
	cw.s.gr.mu.Unlock()
	q := url.Values{
		"client_id":     {c.AppID},
		"redirect_uri":  {cw.s.publicURL(r) + "/oauth/callback"},
		"state":         {cw.key.signed("state", string(st), time.Now().Add(stateTTL))},
		"response_type": {"code"},
	}
	if id := os.Getenv("FB_LOGIN_CONFIG_ID"); id != "" {
		q.Set("config_id", id)
	} else {
		q.Set("scope", strings.Join(loginScopes, ","))
	}
	http.Redirect(w, r, "https://www.facebook.com/"+version+"/dialog/oauth?"+q.Encode(), http.StatusFound)
}

// callback finishes Facebook Login: code -> token -> long-lived token ->
// account and its Pages.
func (cw *connectWeb) callback(w http.ResponseWriter, r *http.Request) {
	if !cw.signedIn(r) {
		cw.render(w, http.StatusUnauthorized, connectView{Error: "Phiên đăng nhập đã hết hạn. Đăng nhập lại rồi bấm Kết nối Facebook."})
		return
	}
	q := r.URL.Query()
	raw, ok := cw.key.verify("state", q.Get("state"))
	if !ok {
		cw.render(w, http.StatusBadRequest, connectView{SignedIn: true, Error: "Yêu cầu kết nối không hợp lệ hoặc đã quá 10 phút. Bấm Kết nối Facebook lại."})
		return
	}
	if e := q.Get("error"); e != "" {
		cw.render(w, http.StatusOK, connectView{SignedIn: true, Error: "Facebook từ chối hoặc bạn đã huỷ: " + firstNonEmpty(q.Get("error_description"), e)})
		return
	}
	var st oauthState
	json.Unmarshal([]byte(raw), &st)

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), cw.s.timeout)
	defer cancel()
	c, err := cw.s.prepare()
	if err == nil {
		var out map[string]any
		out, err = cw.s.gr.call(ctx, "GET", "oauth/access_token", "", url.Values{
			"client_id": {c.AppID}, "client_secret": {c.AppSecret},
			"redirect_uri": {cw.s.publicURL(r) + "/oauth/callback"}, "code": {q.Get("code")},
		})
		if err == nil {
			var a account
			a, err = cw.s.connectAccount(ctx, c, str(out["access_token"]), "oauth", st.Label)
			if err == nil {
				var res map[string]any
				res, err = cw.s.refreshPages(ctx, c, a.ID)
				if err == nil {
					v := connectView{SignedIn: true, Connected: &a, Notice: "Đã kết nối " + a.Name}
					v.Pages, _ = res["pages"].([]publicPage)
					v.Accounts, _ = cw.s.store.publicAccounts()
					if e, ok := res["errors"]; ok {
						b, _ := json.Marshal(e)
						v.Error = "Không lấy được Page: " + string(b)
					}
					cw.render(w, http.StatusOK, v)
					return
				}
			}
		}
	}
	cw.render(w, http.StatusOK, connectView{SignedIn: true, Error: "Kết nối thất bại: " + err.Error()})
}

func (cw *connectWeb) remove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !cw.sameOrigin(r) || !cw.signedIn(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	a, n, err := cw.s.store.removeAccount(r.PostFormValue("account_id"))
	if err != nil {
		cw.render(w, http.StatusOK, connectView{SignedIn: true, Error: err.Error()})
		return
	}
	http.Redirect(w, r, "/connect?notice="+url.QueryEscape("Đã gỡ "+a.Name+" và "+strconv.Itoa(n)+" Page"), http.StatusSeeOther)
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

var connectTmpl = template.Must(template.New("connect").Parse(`<!doctype html>
<html lang="vi"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Kết nối tài khoản</title>
<style>
:root{--bg:#fff;--fg:#1c1e21;--muted:#65676b;--line:#dadde1;--card:#f0f2f5;--accent:#1877f2;--ok:#e7f3e8;--bad:#fdecea}
@media (prefers-color-scheme:dark){:root{--bg:#18191a;--fg:#e4e6eb;--muted:#b0b3b8;--line:#3e4042;--card:#242526;--ok:#1f3a24;--bad:#3a1f1f}}
body{font:15px/1.55 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;max-width:720px;margin:0 auto;padding:24px 16px;color:var(--fg);background:var(--bg)}
h1{font-size:22px;margin:0 0 16px}h2{font-size:17px;margin:24px 0 8px}
.card{background:var(--card);border-radius:8px;padding:12px 14px;margin:8px 0}
.msg{border-radius:8px;padding:10px 12px;margin:12px 0}.ok{background:var(--ok)}.bad{background:var(--bad)}
input[type=text],input[type=password]{width:100%;box-sizing:border-box;padding:8px 10px;font:inherit;border:1px solid var(--line);border-radius:6px;background:var(--bg);color:var(--fg)}
button{padding:8px 14px;font:inherit;border:0;border-radius:6px;background:var(--accent);color:#fff;cursor:pointer}
button.link{background:none;color:var(--muted);padding:0;text-decoration:underline}
.row{display:flex;gap:8px;align-items:center;justify-content:space-between;flex-wrap:wrap}
.muted{color:var(--muted);font-size:13px}ul{padding-left:20px}code{font-size:13px;word-break:break-all}
</style></head><body>
<h1>Kết nối tài khoản Facebook</h1>
{{if .Error}}<div class="msg bad">{{.Error}}</div>{{end}}
{{if .Notice}}<div class="msg ok">{{.Notice}}</div>{{end}}

{{if not .SignedIn}}
<form method="post" action="/connect/login" class="card">
<label for="pw">Mật khẩu (FBP_AUTH_TOKEN)</label>
<p><input id="pw" name="password" type="password" autocomplete="current-password" required autofocus></p>
<button>Đăng nhập</button>
</form>
{{else}}

{{if .Connected}}
<h2>Page của {{.Connected.Name}} ({{len .Pages}})</h2>
<ul>{{range .Pages}}<li>{{.Name}} <span class="muted">{{.ID}}</span></li>{{end}}</ul>
<p><a href="/connect">← Quay lại danh sách tài khoản</a></p>
{{else}}

<h2>Tài khoản đã kết nối</h2>
{{range .Accounts}}
<div class="card row">
<div><b>{{.Name}}</b>{{if .Label}} · {{.Label}}{{end}}<br><span class="muted">{{.Pages}} Page · kết nối {{.ConnectedAt}} · {{.Source}}</span></div>
<form method="post" action="/connect/remove" onsubmit="return confirm('Gỡ {{.Name}} và toàn bộ Page của tài khoản này khỏi server?')">
<input type="hidden" name="account_id" value="{{.ID}}"><button class="link">Gỡ</button></form>
</div>
{{else}}<p class="muted">Chưa có tài khoản nào.</p>{{end}}

<h2>Thêm tài khoản</h2>
<form method="get" action="/connect/start" class="card">
<label for="label">Ghi chú (tuỳ chọn), ví dụ: Bất động sản</label>
<p><input id="label" name="label" type="text" maxlength="60"></p>
<button>Kết nối Facebook</button>
<p class="muted">Đăng nhập đúng tài khoản Facebook cần thêm trước khi bấm. Tài khoản đó phải có vai trò trong app (Admin/Developer/Tester) và quyền đăng bài trên Page. Ở bước chọn Page, hãy chọn tất cả Page muốn đăng.</p>
</form>
<p class="muted">URL chuyển hướng cần khai báo trong Facebook Login của app: <code>{{.RedirectURI}}</code>{{if .ConfigID}} · đang dùng FB_LOGIN_CONFIG_ID{{end}}</p>
{{end}}

<form method="post" action="/connect/logout"><button class="link">Đăng xuất</button></form>
{{end}}
</body></html>`))
