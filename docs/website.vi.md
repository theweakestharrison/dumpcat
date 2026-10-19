# Deploy dumpcat.cc và downloads.dumpcat.cc (Cloudflare)

| Địa chỉ | Nội dung | Chạy trên | Lấy từ |
|---|---|---|---|
| `https://dumpcat.cc` | Landing page, kiêm trang release (mục **Tải về**) | Cloudflare Worker `dumpcat-site` (chỉ phục vụ file tĩnh), cấu hình trong `wrangler.jsonc` | thư mục `site/` |
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

Lần đầu dùng, bạn phải bật R2 trong dashboard (**R2 Object Storage**). Cloudflare yêu cầu thêm phương thức thanh toán, nhưng 10 GB lưu
trữ đầu tiên được miễn phí và R2 không tính phí băng thông tải xuống. Phần còn lại làm bằng wrangler, chạy từ thư mục gốc của repo, sau
`npx wrangler login`:

1. Tạo bucket:

   ```bash
   npx wrangler r2 bucket create dumpcat-releases
   ```

2. **CORS**, để landing page đọc được `latest.json`. Dòng `localhost:8642` dùng để xem thử site trên máy. Lưu nội dung sau vào
   `r2-cors.json` (đặt ở ngoài repo cũng được):

   ```json
   {
     "rules": [
       {
         "allowed": {
           "origins": ["https://dumpcat.cc", "https://www.dumpcat.cc", "http://localhost:8642"],
           "methods": ["GET", "HEAD"]
         },
         "maxAgeSeconds": 3600
       }
     ]
   }
   ```

   ```bash
   npx wrangler r2 bucket cors set dumpcat-releases --file r2-cors.json
   ```

3. **Gắn domain.** Zone ID nằm ở cột bên phải trang **Overview** của `dumpcat.cc` trên dashboard. Lệnh này tự tạo bản ghi DNS và chứng
   chỉ HTTPS:

   ```bash
   npx wrangler r2 bucket domain add dumpcat-releases --domain downloads.dumpcat.cc --zone-id <ZONE_ID> --min-tls 1.2
   ```

   Hoặc làm trên dashboard: bucket → **Settings → Custom Domains → Add**. Để **Public Development URL** (r2.dev) ở trạng thái tắt.
4. *(Khuyến nghị)* **Không cache `latest.json`.** Mặc định Cloudflare không cache file `.json`, và `scripts/publish-release.sh` đã gắn
   `Cache-Control: no-cache` cho nó. Rule này chỉ để chắc chắn hơn: vào domain `dumpcat.cc` → **Caching → Cache Rules → Create rule** →
   **Edit expression**:

   ```
   (http.host eq "downloads.dumpcat.cc" and http.request.uri.path in {"/latest.json" "/latest.json.sig"})
   ```

   **Cache eligibility: Bypass cache** → **Deploy**. File trong thư mục phiên bản không bao giờ đổi nên được cache thoải mái.
   `latest.json` thì đổi mỗi lần phát hành: nếu bị cache, máy cài sẽ thấy bản mới trễ, hoặc thấy `latest.json` lệch với file `.sig`.
5. *(Tùy chọn)* **Rules → Redirect Rules → Create rule** để ai mở `downloads.dumpcat.cc` được chuyển về trang tải:
   expression `(http.host eq "downloads.dumpcat.cc" and http.request.uri.path eq "/")` → **Static** `https://dumpcat.cc/#download`,
   mã 302.

### 3. Landing page trên Cloudflare Workers

Từ Wrangler 4, Cloudflare gộp Pages vào Workers: lệnh `wrangler pages project create` không còn tạo project Pages nữa. File
`wrangler.jsonc` ở thư mục gốc của repo khai báo Worker `dumpcat-site`. Worker này không có code, chỉ phục vụ các file trong `site/`, và
được gắn domain `dumpcat.cc`. Chạy từ thư mục gốc của repo:

```bash
npx wrangler login
npx wrangler deploy
```

- Lệnh `deploy` upload `site/`, tự tạo bản ghi DNS và chứng chỉ HTTPS cho `dumpcat.cc`. Nếu `dumpcat.cc` đã có bản ghi DNS khác (ví dụ
  bản ghi A do nhà đăng ký tạo sẵn), xóa bản ghi đó trước rồi chạy lại.
- `site/_headers` đặt Content-Security-Policy và các header bảo mật khác. Bản thân file đó không được phục vụ ra ngoài.
  CSP cho phép Cloudflare Web Analytics, vì Cloudflare tự chèn script này vào trang.
- Worker không có địa chỉ `*.workers.dev` (`workers_dev: false`), chỉ chạy ở `dumpcat.cc`.
- *(Tùy chọn)* `www.dumpcat.cc`: trong DNS tạo bản ghi `AAAA www 100::` bật **Proxied**. Sau đó vào **Rules → Redirect Rules**, dùng
  mẫu **Redirect from WWW to root** (301).

Mở `https://dumpcat.cc`: mục **Tải về** phải hiện phiên bản mới nhất. Khi R2 còn trống, mục đó báo "Chưa tải được thông tin bản phát
hành". Sau lần phát hành đầu tiên (Phần 2), trang sẽ tự hiện đúng.

---

## Phần 2 — Mỗi lần phát hành

1. Đặt phiên bản trong `VERSION` và viết mục `## <phiên bản>` trong `CHANGELOG.md`. Mục này trở thành release notes trên trang và trong
   ứng dụng. Đổi luôn số phiên bản trong các lệnh tải và cài ở `README.md`, `README.vi.md`, `docs/manual.md` và `docs/manual.vi.md`
   (`grep -rn "1\.0\.0" README*.md docs/manual*.md`), vì các file này nằm trong gói cài. Landing page thì tự cập nhật.
2. Build có ký bằng khóa tạo bởi `dumpcat release keygen` (giữ ngoài repo, có bản sao offline; xem
   `docs/manual.vi.md`, mục Cập nhật). Trong Git Bash:

   ```bash
   UPDATE_URL=https://downloads.dumpcat.cc UPDATE_SIGNING_KEY=/path/to/dumpcat-release.key ./build.sh
   ```

   Trong PowerShell: `.\build.ps1 -UpdateUrl https://downloads.dumpcat.cc -SigningKey <đường dẫn>\dumpcat-release.key`.
3. Kiểm tra chữ ký trước khi upload:

   ```bash
   dist/dumpcat-windows-amd64.exe release verify -pub /path/to/dumpcat-release.pub dist/update/latest.json
   ```

4. Upload:

   ```bash
   scripts/publish-release.sh
   ```

   Script upload theo đúng thứ tự: thư mục phiên bản trước, rồi `latest.json.sig`, cuối cùng `latest.json`. Nhờ vậy máy cài không bao giờ
   thấy một bản phát hành mà file chưa có trên server. Script cũng gắn `Cache-Control` cho từng file, và từ chối chạy nếu phiên bản này đã
   được phát hành với file khác. Từ PowerShell, chạy `bash scripts/publish-release.sh`.
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
| `site/_headers` | Content-Security-Policy và các header bảo mật |
| `site/favicon.svg` | Bản sao của `web/assets/favicon.svg` |
| `site/og.png` | Ảnh xem trước khi chia sẻ link (Open Graph, 1200×630), được render từ `site/og-image.html` |
| `site/og-image.html` | Nguồn của `og.png`. Không được deploy vì có tên trong `site/.assetsignore` |
| `wrangler.jsonc` | Worker `dumpcat-site`: thư mục `site/` và domain `dumpcat.cc` |

Trang hiện tiếng Anh mặc định. Lựa chọn EN/VI của người xem được lưu trong trình duyệt của họ. Font tải từ Google Fonts (Be Vietnam Pro,
Bricolage Grotesque, JetBrains Mono).

**Xem thử trên máy:**

```bash
python -m http.server 8642 --directory site
```

Mở `http://localhost:8642`. Mục Tải về đọc `latest.json` thật trên `downloads.dumpcat.cc`, nhờ dòng `localhost:8642` trong CORS.
Lưu ý: khi chạy thử trên máy, `_headers` không được áp dụng.

**Deploy lại** sau khi sửa: chạy `npx wrangler deploy` ở thư mục gốc của repo. Nếu bản mới có lỗi, quay lại bản trước bằng
`npx wrangler rollback`.

**Lưu ý thêm:**

- Số `1.0.0` trong HTML chỉ là giá trị dự phòng, hiện trong lúc trang đang tải `latest.json` hoặc khi tải lỗi. Script tự thay bằng phiên
  bản thật.
- Muốn đổi địa chỉ máy chủ phát hành thì sửa cả hai chỗ: `<meta name="dumpcat-release-base">` trong `index.html` và `connect-src`
  trong `_headers`.
- **Sửa ảnh chia sẻ:** sửa `site/og-image.html`, render lại bằng Chrome, rồi deploy:

  ```bash
  "/c/Program Files/Google/Chrome/Application/chrome.exe" --headless=new --hide-scrollbars --force-device-scale-factor=1 \
    --window-size=1200,630 --virtual-time-budget=8000 --screenshot="$PWD/site/og.png" "file:///$PWD/site/og-image.html"
  ```

  Facebook, Zalo, X và Slack lưu cache ảnh xem trước. Để chúng lấy ảnh mới, dán link vào
  [Facebook Sharing Debugger](https://developers.facebook.com/tools/debug/) và bấm **Scrape Again**.

---

## Phần 4 — Trang so sánh (SEO)

Các trang `https://dumpcat.cc/compare/…` (tiếng Anh) và `https://dumpcat.cc/vi/compare/…` (tiếng Việt) được tạo từ dữ liệu, không
viết tay:

| File | Nội dung |
|---|---|
| `site-src/compare/<slug>.json` | Một công cụ: tên, giá trị từng dòng của bảng so sánh, "chọn họ nếu" / "chọn DumpCat nếu", các bước chuyển, nguồn, ngày kiểm tra |
| `scripts/build-site.py` | Template, cột DumpCat (dùng chung cho mọi trang), sinh trang so sánh EN/VI, `sitemap.xml` và `robots.txt` |

Sửa hoặc thêm một công cụ:

1. Sửa (hoặc chép từ file có sẵn) `site-src/compare/<slug>.json`. Mọi thông tin về sản phẩm khác **phải có nguồn** trong
   `sources`. Điều gì chưa kiểm chứng được thì ghi "Not documented on the pages we checked" chứ không đoán. Cập nhật `checked`.
2. Chạy `python scripts/build-site.py` (chỉ dùng thư viện chuẩn của Python), xem thử bằng `python -m http.server 8642 --directory site`.
3. Deploy: `npx wrangler deploy`.

Nên kiểm tra lại giá và tính năng của các công cụ khoảng 6 tháng một lần. Sau lần deploy đầu, thêm `https://dumpcat.cc/sitemap.xml` vào
Google Search Console và Bing Webmaster Tools.
