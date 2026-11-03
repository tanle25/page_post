# Chuyển app Facebook sang Live (không cần App Review)

Checklist này dùng cho trường hợp **chỉ đăng lên các Page của chính bạn**. Mọi người lấy token đều phải có vai trò trong app. Khi app ở chế độ Live với **Standard Access** thì không cần App Review, cũng không cần xác minh doanh nghiệp.

> **Nếu giao cho Claude Cowork làm:** bạn tự đăng nhập Facebook hoặc Meta for Developers và tự nhập mã 2FA. Bấm **Save** cho từng mục thì để Cowork làm. Riêng nút **Publish / Go Live** ở bước 5 thì bạn xem lại toàn bộ rồi **tự bấm**. Không bấm **Request advanced access** hay **Start App Review**.

## Bước 0: Chuẩn bị trang chính sách
- [ ] Trong Vercel, vào **Settings → Environment Variables**, thêm:
  - `FBP_OWNER_NAME`: tên shop hoặc tên của bạn, sẽ hiện trên trang chính sách.
  - `FBP_CONTACT_EMAIL`: email nhận yêu cầu xoá dữ liệu.
- [ ] Deploy lại.
- [ ] Mở và kiểm tra hai trang dưới đây. Cả hai phải hiện đúng tên và email, không còn dòng "chưa cấu hình":
  - `https://<project>.vercel.app/privacy`
  - `https://<project>.vercel.app/data-deletion`

## Bước 1: Thông tin cơ bản của app
Vào developers.facebook.com, chọn app, mở **App settings → Basic**:
- [ ] **Privacy Policy URL**: `https://<project>.vercel.app/privacy`
- [ ] **User data deletion**: chọn kiểu **Data deletion instructions URL**, điền `https://<project>.vercel.app/data-deletion`
- [ ] **App icon**: ảnh vuông 1024×1024, ví dụ logo shop.
- [ ] **Category**: chọn mục gần nhất, ví dụ *Business and pages*.
- [ ] **Contact email**: email bạn thường kiểm tra.
- [ ] Bấm **Save changes**.

## Bước 2: Vai trò trong app
Vào **App roles → Roles**:
- [ ] Mọi tài khoản dùng để lấy token phải là **Administrator**, **Developer** hoặc **Tester**.
- [ ] Người được mời phải **chấp nhận lời mời** tại developers.facebook.com → **Requests**. Lời mời còn treo thì chưa tính.
- [ ] Trên **Page**, tài khoản đó phải có quyền đăng nội dung. Nếu `fbp_list_pages` hiện các quyền của tài khoản trên Page, trong đó phải có `CREATE_CONTENT`.

## Bước 3: Quyền (permissions)
Mở **Use cases** (hoặc **App Review → Permissions and features** ở giao diện cũ):
- [ ] Có use case quản lý Page, ví dụ *Manage everything on your Page*.
- [ ] Đủ các quyền sau, để ở mức **Standard Access** (mặc định):
  - `pages_show_list`
  - `pages_read_engagement`
  - `pages_manage_posts`
  - `pages_manage_engagement`
  - `read_insights`
- [ ] **Không** bấm *Request advanced access*.

## Bước 4: Kiểm tra trước khi Live
- [ ] Mục **Required actions** hoặc **Publish** không còn cảnh báo đỏ (thiếu privacy URL, icon, v.v.).
- [ ] Nếu Meta yêu cầu **Business verification** chỉ để chuyển sang Live, **dừng lại và báo Claude** trước khi nộp giấy tờ. Trường hợp dùng Standard Access thường không bị yêu cầu bước này.

## Bước 5: Chuyển sang Live
- [ ] Gạt **App Mode** từ *Development* sang **Live**. Ở giao diện mới, đây là nút **Publish** hoặc **Go live**.
- [ ] Bạn tự bấm sau khi đã xem lại các bước 1–4.

## Bước 6: Lấy token mới và kiểm tra
- [ ] Mở **Graph API Explorer**, chọn app, bấm **Get User Access Token**, tick 5 quyền ở bước 3, rồi copy token.
- [ ] Cập nhật `FB_USER_TOKEN` trên Vercel (hoặc `user_token` trong `~/.facebook-go/page-config.json` nếu chạy local).
  - Không dán token vào chat.
  - Nếu sửa trên Vercel thì deploy lại.
- [ ] Nhờ Claude chạy `fbp_refresh_pages` rồi `fbp_status`.
- [ ] Đăng một bài thử có ảnh bằng `fbp_create_post`.
- [ ] Mở link bài bằng **cửa sổ ẩn danh**, hoặc nhờ một người không có vai trò trong app mở. Phải thấy đủ **chữ và ảnh**.

## Khi có người yêu cầu xoá dữ liệu
- Nếu yêu cầu là từ chính bạn: nhờ Claude chạy `fbp_delete_stored_data` với `confirm: "DELETE"`. Lệnh này xoá token, danh sách Page, nhật ký và trạng thái dừng ghi.
- Gửi email xác nhận cho người yêu cầu trong vòng 30 ngày, như đã cam kết trên trang `/data-deletion`.
