package main

import (
	"html/template"
	"net/http"
	"os"
	"strings"
)

// Public pages Meta asks for before an app can go Live: a privacy policy and
// data deletion instructions. They describe exactly what this server stores.
// The owner and contact come from FBP_OWNER_NAME and FBP_CONTACT_EMAIL so no
// personal details live in the code.

const policyEffective = "2026-11-03"

type policyData struct {
	Owner, Email, Effective, Host string
}

func policyFromEnv(r *http.Request) policyData {
	d := policyData{Owner: os.Getenv("FBP_OWNER_NAME"), Email: os.Getenv("FBP_CONTACT_EMAIL"), Effective: policyEffective, Host: r.Host}
	if strings.TrimSpace(d.Owner) == "" {
		d.Owner = "[FBP_OWNER_NAME chưa cấu hình]"
	}
	if strings.TrimSpace(d.Email) == "" {
		d.Email = "[FBP_CONTACT_EMAIL chưa cấu hình]"
	}
	return d
}

func servePolicy(t *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		t.Execute(w, policyFromEnv(r))
	}
}

const policyHead = `<!doctype html>
<html lang="vi"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{template "title" .}}</title>
<style>
body{font:16px/1.6 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;max-width:760px;margin:0 auto;padding:24px 16px;color:#1c1e21;background:#fff}
h1{font-size:26px}h2{font-size:19px;margin-top:28px}hr{margin:40px 0;border:0;border-top:1px solid #ddd}
.meta{color:#65676b;font-size:14px}code{background:#f0f2f5;padding:1px 4px;border-radius:4px}
</style></head><body>`

var privacyTmpl = template.Must(template.New("privacy").Parse(policyHead + `{{define "title"}}Chính sách quyền riêng tư · Privacy Policy{{end}}
<h1>Chính sách quyền riêng tư</h1>
<p class="meta">Chủ sở hữu: {{.Owner}} · Liên hệ: {{.Email}} · Hiệu lực từ {{.Effective}}</p>

<p>Ứng dụng này là công cụ nội bộ giúp {{.Owner}} đăng và quản lý bài viết trên các Trang Facebook do chính {{.Owner}} quản trị, thông qua Graph API chính thức của Meta. Ứng dụng không dành cho công chúng và không thu thập dữ liệu của khách truy cập Trang.</p>

<h2>Dữ liệu được lưu</h2>
<ul>
<li><b>Mã truy cập (access token)</b> của tài khoản quản trị và của từng Trang, do Meta cấp khi quản trị viên cho phép ứng dụng.</li>
<li><b>Thông tin Trang:</b> ID, tên, danh mục và quyền của quản trị viên trên Trang.</li>
<li><b>Nhật ký thao tác:</b> thời điểm, Trang, nội dung bài viết hoặc bình luận đã đăng, đường dẫn ảnh, ID bài và kết quả thành công hay lỗi.</li>
</ul>
<p>Ứng dụng có đọc bài viết, bình luận và số liệu thống kê của Trang khi được yêu cầu, nhưng chỉ để hiển thị cho quản trị viên và không lưu lại.</p>

<h2>Mục đích sử dụng</h2>
<p>Dữ liệu chỉ dùng để đăng, chỉnh sửa và xoá bài; trả lời và kiểm duyệt bình luận; xem thống kê trên các Trang của {{.Owner}}. Không dùng cho quảng cáo, không lập hồ sơ người dùng và không bán dữ liệu.</p>

<h2>Lưu trữ và chia sẻ</h2>
<p>Dữ liệu được lưu trong cơ sở dữ liệu riêng (Upstash Redis) của máy chủ ứng dụng trên Vercel, hoặc trên máy tính của quản trị viên. Mã truy cập không bao giờ được hiển thị hay gửi cho bên thứ ba. Dữ liệu chỉ được gửi tới Meta (graph.facebook.com) để thực hiện các thao tác trên. Trang web này không dùng cookie hay công cụ theo dõi.</p>

<h2>Thời gian lưu</h2>
<p>Mã truy cập và thông tin Trang được lưu cho tới khi quản trị viên gỡ ứng dụng hoặc yêu cầu xoá. Nhật ký giữ tối đa 10.000 thao tác gần nhất.</p>

<h2>Quyền của bạn</h2>
<p>Bạn có thể thu hồi quyền của ứng dụng bất cứ lúc nào trong phần cài đặt Facebook, và yêu cầu xoá dữ liệu theo <a href="/data-deletion">hướng dẫn xoá dữ liệu</a>. Mọi câu hỏi xin gửi tới {{.Email}}.</p>

<hr>

<h1>Privacy Policy</h1>
<p class="meta">Owner: {{.Owner}} · Contact: {{.Email}} · Effective {{.Effective}}</p>

<p>This app is an internal tool that lets {{.Owner}} publish and manage posts on the Facebook Pages {{.Owner}} administers, through Meta's official Graph API. It is not offered to the public and does not collect data about people who visit those Pages.</p>

<h2>Data we store</h2>
<ul>
<li><b>Access tokens</b> for the administrator's account and for each Page, issued by Meta when the administrator authorizes the app.</li>
<li><b>Page details:</b> ID, name, category and the administrator's tasks on the Page.</li>
<li><b>An action log:</b> time, Page, the text of posts or comments published, image links, resulting IDs, and whether the action succeeded.</li>
</ul>
<p>Posts, comments and insights are read on request to show them to the administrator and are not stored.</p>

<h2>How we use it</h2>
<p>Only to publish, edit and delete posts, reply to and moderate comments, and view insights on {{.Owner}}'s Pages. Data is not used for advertising or profiling and is never sold.</p>

<h2>Storage and sharing</h2>
<p>Data is kept in the app server's private database (Upstash Redis) on Vercel, or on the administrator's own computer. Access tokens are never displayed or shared with third parties. Data is sent only to Meta (graph.facebook.com) to carry out the actions above. This website sets no cookies and uses no trackers.</p>

<h2>Retention</h2>
<p>Tokens and Page details are kept until the administrator removes the app or asks for deletion. The log keeps the most recent 10,000 actions.</p>

<h2>Your choices</h2>
<p>You can revoke the app's access at any time in your Facebook settings and request deletion by following the <a href="/data-deletion">data deletion instructions</a>. Questions: {{.Email}}.</p>
</body></html>`))

var deletionTmpl = template.Must(template.New("deletion").Parse(policyHead + `{{define "title"}}Xoá dữ liệu · Data Deletion{{end}}
<h1>Hướng dẫn xoá dữ liệu</h1>
<p class="meta">{{.Owner}} · {{.Email}}</p>

<h2>1. Gỡ ứng dụng khỏi Facebook</h2>
<p>Vào <b>Cài đặt và quyền riêng tư → Cài đặt → Ứng dụng và trang web</b> (hoặc <b>Tích hợp doanh nghiệp</b>), chọn ứng dụng này và bấm <b>Gỡ</b>. Mọi mã truy cập ứng dụng đang giữ sẽ mất hiệu lực ngay lập tức.</p>

<h2>2. Yêu cầu xoá dữ liệu đã lưu</h2>
<p>Gửi email tới <b>{{.Email}}</b> với tiêu đề <i>"Yêu cầu xoá dữ liệu"</i>, kèm tên tài khoản Facebook hoặc ID Trang. Toàn bộ mã truy cập, thông tin Trang và nhật ký thao tác liên quan sẽ bị xoá trong vòng 30 ngày, và bạn sẽ nhận được email xác nhận.</p>

<hr>

<h1>Data Deletion Instructions</h1>

<h2>1. Remove the app from Facebook</h2>
<p>Go to <b>Settings &amp; privacy → Settings → Apps and websites</b> (or <b>Business integrations</b>), select this app and click <b>Remove</b>. Every access token the app holds stops working immediately.</p>

<h2>2. Ask us to delete stored data</h2>
<p>Email <b>{{.Email}}</b> with the subject <i>"Data deletion request"</i> and your Facebook account name or Page ID. All access tokens, Page details and action logs relating to you will be deleted within 30 days, and you will receive a confirmation email.</p>
</body></html>`))
