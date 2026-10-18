# Deploy dumpcat.cc và downloads.dumpcat.cc (Cloudflare)

| Địa chỉ | Nội dung | Chạy trên | Lấy từ |
|---|---|---|---|
| `https://dumpcat.cc` | Landing page, kiêm trang release (mục **Tải về**) | Cloudflare Pages, project `dumpcat` | thư mục `site/` |
| `https://downloads.dumpcat.cc` | `latest.json` và các thư mục phiên bản, cho trình cập nhật và nút tải | Cloudflare R2, bucket `dumpcat-releases` | `dist/update/` sau khi build |

Mỗi lần mở, landing page đọc `https://downloads.dumpcat.cc/latest.json` để hiện phiên bản, release notes, link tải và SHA-256.
Vì vậy phát hành bản mới **chỉ cần upload lên R2**, không phải deploy lại site.

---

## Phần 1 — Cài đặt (làm một lần)

### 1. Đưa dumpcat.cc vào Cloudflare

1. [dash.cloudflare.com](https://dash.cloudflare.com) → **Add a domain** → `dumpcat.cc` → gói **Free**.
2. Ở nhà đăng ký tên miền, đổi nameserver sang hai nameserver Cloudflare đưa ra. Chờ domain chuyển sang **Active** (vài phút
   đến vài giờ).
3. **SSL/TLS → Edge Certificates** → bật **Always Use HTTPS**.

### 2. Máy chủ phát hành trên R2

1. **R2 Object Storage**. Lần đầu dùng, bạn phải kích hoạt R2: Cloudflare yêu cầu thêm phương thức thanh toán, nhưng 10 GB lưu trữ đầu
   tiên được miễn phí và R2 không tính phí băng thông tải xuống. Sau đó **Create bucket** → tên `dumpcat-releases`, location **Automatic**.
2. Bucket → **Settings → Custom Domains → Add** → `downloads.dumpcat.cc` → **Connect domain**. Cloudflare tự tạo bản ghi DNS và
   chứng chỉ HTTPS. Để **Public Development URL** (r2.dev) ở trạng thái tắt.
3. **Settings → CORS policy → Add CORS policy**, dán nội dung sau. Nó cho phép landing page đọc `latest.json`; dòng `localhost:8642`
   dùng để xem thử site trên máy:

   ```json
   [
     {
       "AllowedOrigins": ["https://dumpcat.cc", "https://www.dumpcat.cc", "http://localhost:8642"],
       "AllowedMethods": ["GET", "HEAD"],
       "MaxAgeSeconds": 3600
     }
   ]
   ```

4. **Không cache `latest.json`.** Vào domain `dumpcat.cc` → **Caching → Cache Rules → Create rule** → **Edit expression**:

   ```
   (http.host eq "downloads.dumpcat.cc" and http.request.uri.path in {"/latest.json" "/latest.json.sig"})
   ```

   **Cache eligibility: Bypass cache** → **Deploy**. File trong thư mục phiên bản không bao giờ đổi nên được cache thoải mái.
   `latest.json` thì đổi mỗi lần phát hành: nếu bị cache, máy cài sẽ thấy bản mới trễ, hoặc thấy `latest.json` lệch với file `.sig`.
5. *(Tùy chọn)* **Rules → Redirect Rules → Create rule** để ai mở `downloads.dumpcat.cc` được chuyển về trang tải:
   expression `(http.host eq "downloads.dumpcat.cc" and http.request.uri.path eq "/")` → **Static** `https://dumpcat.cc/#download`,
   mã 302.
6. **Token để upload.** **R2 → Manage API tokens → Create API token** → quyền **Object Read & Write** → **Apply to specific buckets
   only** → chọn `dumpcat-releases` → **Create**. Chép lại **Access Key ID**, **Secret Access Key** (secret chỉ hiện một lần) và
   endpoint S3 `https://<ACCOUNT_ID>.r2.cloudflarestorage.com`.
7. **Cấu hình rclone trên máy build.** Chạy `rclone config`, tạo remote tên `r2`, chọn storage **s3**, provider **Cloudflare**, rồi nhập
   key, secret và endpoint vừa chép. Kết quả trong `rclone.conf`:

   ```ini
   [r2]
   type = s3
   provider = Cloudflare
   access_key_id = <Access Key ID>
   secret_access_key = <Secret Access Key>
   endpoint = https://<ACCOUNT_ID>.r2.cloudflarestorage.com
   acl = private
   no_check_bucket = true
   ```

   Cần `no_check_bucket = true` vì token chỉ có quyền trên object, không có quyền tạo bucket. Thử bằng `rclone ls r2:dumpcat-releases`:
   lệnh không báo lỗi là được (bucket đang trống).

### 3. Landing page trên Cloudflare Pages

Cách A: dùng wrangler. Máy này đã có Node.js, chạy từ thư mục gốc của repo:

```bash
npx wrangler login
npx wrangler pages project create dumpcat --production-branch main
npx wrangler pages deploy site --project-name dumpcat --branch main
```

Cách B: dùng dashboard. **Workers & Pages → Create → Pages → Upload assets** → đặt tên `dumpcat` → kéo thả thư mục `site` → **Deploy**.

Sau đó gắn domain:

1. Project `dumpcat` → **Custom domains → Set up a custom domain** → `dumpcat.cc` → **Activate domain**.
2. Làm lại với `www.dumpcat.cc`. Sau đó vào **Rules → Redirect Rules**, dùng mẫu **Redirect from WWW to root** để chuyển
   `www.dumpcat.cc` về `https://dumpcat.cc` (301).
3. Mở `https://dumpcat.cc`: mục **Tải về** phải hiện phiên bản mới nhất. Lúc này R2 còn trống nên mục đó báo "Chưa tải được thông tin
   bản phát hành". Sau khi phát hành lần đầu (Phần 2), trang sẽ tự hiện đúng.

`site/_headers` đặt Content-Security-Policy và các header bảo mật khác, Pages tự áp dụng. Địa chỉ `*.pages.dev` cũng báo "Chưa tải được"
ở mục Tải về, vì CORS chỉ cho phép `dumpcat.cc`. Đó là điều bình thường.

---

## Phần 2 — Mỗi lần phát hành

1. Đặt phiên bản trong `VERSION` và viết mục `## <phiên bản>` trong `CHANGELOG.md`. Mục này trở thành release notes trên trang và trong
   ứng dụng.
2. Build. Trên Windows dùng PowerShell:

   ```powershell
   .\build.ps1 -UpdateUrl https://downloads.dumpcat.cc -SigningKey D:\keys\dumpcat-release.key
   ```

   Trên Linux: `UPDATE_SIGNING_KEY=~/dumpcat-keys/dumpcat-release.key UPDATE_URL=https://downloads.dumpcat.cc ./build.sh`.
   Nếu chưa tạo khóa ký thì bỏ phần khóa; cách tạo khóa có trong `docs/manual.vi.md`, mục Cập nhật.
3. Nếu có ký, kiểm tra chữ ký trước khi upload:

   ```powershell
   dist\dumpcat-windows-amd64.exe release verify -pub D:\keys\dumpcat-release.pub dist\update\latest.json
   ```

4. **Upload đúng thứ tự:** thư mục phiên bản trước, rồi `latest.json.sig`, cuối cùng `latest.json`. Nhờ vậy máy cài không bao giờ thấy
   một bản phát hành mà file chưa có trên server. Thay `1.1.0` bằng phiên bản của bạn; các lệnh chạy được cả trong bash lẫn PowerShell:

   ```bash
   rclone copy dist/update/1.1.0 r2:dumpcat-releases/1.1.0 --header-upload "Cache-Control: public, max-age=31536000, immutable"
   rclone copyto dist/update/latest.json.sig r2:dumpcat-releases/latest.json.sig --header-upload "Cache-Control: no-cache"
   rclone copyto dist/update/latest.json r2:dumpcat-releases/latest.json --header-upload "Cache-Control: no-cache"
   ```

   Build không ký thì bỏ lệnh thứ hai.
5. Kiểm tra:
   - `curl.exe -s https://downloads.dumpcat.cc/latest.json` trả về phiên bản mới.
   - Mục **Tải về** trên `https://dumpcat.cc` hiện phiên bản mới, và link tải về được file.
   - Trên một máy đã cài: `dumpcat update -check`.

Hai nguyên tắc:

- **Không upload lại một phiên bản đã phát hành với file khác.** File trong thư mục phiên bản được cache một năm. Muốn sửa thì phát hành
  số phiên bản mới. Nếu bắt buộc phải thay file, purge các URL đó ở **Caching → Configuration → Custom Purge**.
- **Không xóa thư mục phiên bản cũ.** Trang chỉ link bản mới nhất, nhưng người dùng vẫn tải bản cũ theo
  `downloads.dumpcat.cc/<phiên bản>/`.

---

## Phần 3 — Sửa landing page

| File | Nội dung |
|---|---|
| `site/index.html` | Nội dung trang. Chữ tiếng Việt nằm thẳng trong HTML |
| `site/app.js` | Bản tiếng Anh (object `EN`), mô phỏng PITR, tab cài đặt, nút sao chép, mục Tải về đọc `latest.json` |
| `site/style.css` | Giao diện, tự đổi sáng/tối theo hệ điều hành |
| `site/_headers` | Content-Security-Policy và các header bảo mật cho Cloudflare Pages |
| `site/favicon.svg` | Bản sao của `web/assets/favicon.svg` |

Trang hiện tiếng Anh mặc định. Lựa chọn EN/VI của người xem được lưu trong trình duyệt của họ. Font tải từ Google Fonts (Be Vietnam Pro,
Bricolage Grotesque, JetBrains Mono).

**Xem thử trên máy:**

```bash
python -m http.server 8642 --directory site
```

Mở `http://localhost:8642`. Mục Tải về đọc `latest.json` thật trên `downloads.dumpcat.cc`, nhờ dòng `localhost:8642` trong CORS.
Lưu ý: khi chạy thử trên máy, `_headers` không được áp dụng.

**Deploy lại** sau khi sửa: `npx wrangler pages deploy site --project-name dumpcat --branch main`, hoặc tải lại thư mục `site` lên
dashboard.

**Lưu ý thêm:**

- Số `1.0.0` trong HTML chỉ là giá trị dự phòng, hiện trong lúc trang đang tải `latest.json` hoặc khi tải lỗi. Script tự thay bằng phiên
  bản thật.
- Muốn đổi địa chỉ máy chủ phát hành thì sửa cả hai chỗ: `<meta name="dumpcat-release-base">` trong `index.html` và `connect-src`
  trong `_headers`.
