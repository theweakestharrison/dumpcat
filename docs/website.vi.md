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

1. Đặt phiên bản trong `VERSION` và viết mục `## <phiên bản> (<ngày>)` trong `CHANGELOG.md`: một câu tóm tắt, rồi các nhóm
   `### <tên nhóm>`, mỗi nhóm là danh sách `- `. Mục này trở thành release notes trong ứng dụng (kèm link `notes_url` tới trang của
   nó), trang `dumpcat.cc/changelog/<phiên bản>/` và thẻ **Có gì mới** trên landing page (câu tóm tắt và tên các nhóm). Đổi luôn số
   phiên bản trong các lệnh tải và cài ở `docs/manual.md` và `docs/manual.vi.md` (`grep -rn "1\.4\.0" README*.md docs/manual*.md`),
   vì các file này nằm trong gói cài.
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
6. Sinh lại bản demo để nó dùng giao diện của phiên bản mới, sinh lại các trang (trang **Có gì mới** của phiên bản mới, thẻ trên
   landing page), rồi deploy landing page:

   ```bash
   python scripts/build-demo.py
   python scripts/build-site.py
   npx wrangler deploy
   ```

   Nếu upload bản phát hành mà chưa deploy, landing page ẩn thẻ **Có gì mới** của phiên bản cũ cho tới khi deploy.

Hai nguyên tắc:

- **Không upload lại một phiên bản đã phát hành với file khác.** File trong thư mục phiên bản được cache một năm. Muốn sửa thì phát hành
  số phiên bản mới. Nếu bắt buộc phải thay file, purge các URL đó ở **Caching → Configuration → Custom Purge**.
- **Không xóa thư mục phiên bản cũ.** Trang chỉ link bản mới nhất, nhưng người dùng vẫn tải bản cũ theo
  `downloads.dumpcat.cc/<phiên bản>/`.

---

## Phần 3 — Sửa landing page

| File | Nội dung |
|---|---|
| `site-src/landing/index.html` | Khung trang và chữ tiếng Việt. Chỗ nào có chữ thì mang `data-i18n="<khóa>"` (nội dung của phần tử), hoặc `data-i18n-<thuộc tính>="<khóa>"` cho một thuộc tính như `content`, `aria-label` |
| `site-src/landing/en.html` | Chữ tiếng Anh: mỗi khóa một `<template data-i18n="<khóa>">…</template>` |
| `site-src/landing/<mã>.html` | Bản dịch các ngôn ngữ khác (`zh`, `ja`, `ko`, `es`, `pt`, `de`, `fr`): cùng các khóa của `en.html`. Riêng hai hình `c-agent-fig` và `c-wg-fig` dùng `<template data-i18n="…" data-labels>`, mỗi dòng `chữ tiếng Anh => bản dịch`, để hình chỉ viết một lần trong `en.html` |
| `site/index.html`, `site/vi/index.html`, `site/<mã>/index.html` | Do `scripts/build-site.py` sinh từ các file trên: `/` tiếng Anh, `/vi/` tiếng Việt, `/zh/`, `/ja/`… các ngôn ngữ khác. **Không sửa tay**, lần chạy script sau sẽ ghi đè |
| `site/app.js` | Mô phỏng PITR, tab cài đặt, nút sao chép, chọn công cụ trong mục So sánh, mục Tải về đọc `latest.json`. Ngôn ngữ của các câu nó viết ra lấy theo `<html lang>` của trang |
| `site/style.css` | Giao diện, tự đổi sáng/tối theo hệ điều hành |
| `site/_headers` | Content-Security-Policy và các header bảo mật |
| `site/favicon.svg` | Bản sao của `web/assets/favicon.svg` |
| `site/install.sh`, `site/install.ps1` | Lệnh cài một dòng `curl -fsSL https://dumpcat.cc/install.sh \| sudo sh` và `irm https://dumpcat.cc/install.ps1 \| iex`: đọc `latest.json`, tải gói, kiểm tra SHA-256 theo `SHA256SUMS` rồi chạy bộ cài trong gói. `_headers` cho hai file này `Content-Type: text/plain` |
| `site/guide.js` | Nút sao chép và link tải bản mới nhất của trang hướng dẫn cài đặt |
| `site/og.png` | Ảnh xem trước khi chia sẻ link (Open Graph, 1200×630), được render từ `site/og-image.html` |
| `site/og-image.html` | Nguồn của `og.png`. Không được deploy vì có tên trong `site/.assetsignore` |
| `wrangler.jsonc` | Worker `dumpcat-site`: thư mục `site/` và domain `dumpcat.cc` |

Landing page có chín địa chỉ: `https://dumpcat.cc/` (tiếng Anh, cũng là bản `x-default`), `https://dumpcat.cc/vi/` (tiếng Việt) và
`/zh/` (简体中文), `/ja/` (日本語), `/ko/` (한국어), `/es/` (Español), `/pt/` (Português, `pt-BR`), `/de/` (Deutsch), `/fr/` (Français).
Danh sách nằm ở `LANDING_LANGS` trong `scripts/build-site.py`. Mỗi trang là HTML tĩnh đúng một ngôn ngữ: `lang`, title, description,
thẻ chia sẻ (Open Graph, X), JSON-LD, canonical riêng và `hreflang` trỏ qua lại. Vì vậy công cụ tìm kiếm đọc đúng nội dung mà không cần
chạy JavaScript. Menu ngôn ngữ (biểu tượng quả địa cầu) chỉ là các link sang trang kia (giữ nguyên mục đang xem, ví dụ `#download`).
Trang không tự chuyển theo ngôn ngữ trình duyệt, để mỗi địa chỉ luôn trả về đúng một nội dung. Link trong trang đi tới bản cùng ngôn ngữ
nếu có (`/install/` hay `/vi/install/`, `/compare/` hay `/vi/compare/`). Trang so sánh, hướng dẫn cài đặt và **Có gì mới** chỉ có tiếng
Anh và tiếng Việt (Có gì mới chỉ có tiếng Anh), nên các ngôn ngữ khác link tới bản tiếng Anh, và bảng **So sánh** trên landing của các
ngôn ngữ đó là bảng tiếng Anh. Font tải từ Google Fonts (Be Vietnam Pro, Bricolage Grotesque, JetBrains Mono); trang tiếng Trung, Nhật,
Hàn dùng font của hệ điều hành cho chữ CJK (`site/style.css`).

**Sửa chữ trên landing page:**

1. Tiếng Việt: sửa trong `site-src/landing/index.html`. Tiếng Anh: sửa khóa cùng tên trong `site-src/landing/en.html`. Các ngôn ngữ
   khác: sửa khóa cùng tên trong `site-src/landing/<mã>.html`.
2. Thêm một đoạn chữ mới: đặt `data-i18n="<khóa mới>"` lên phần tử trong `index.html`, rồi thêm
   `<template data-i18n="<khóa mới>">…</template>` vào `en.html` và bản dịch vào từng `<mã>.html`. Script dừng và báo lỗi nếu một
   khóa thiếu bản tiếng Anh, `en.html` có khóa không dùng tới, hoặc một `<mã>.html` có khóa lạ. Khóa mà một ngôn ngữ chưa dịch thì trang
   đó dùng tạm chữ tiếng Anh, và script in ra danh sách các khóa đó: dịch nốt trước khi deploy.
3. Chạy `python scripts/build-site.py`, xem thử (bên dưới), rồi commit cả file nguồn lẫn các `site/…/index.html` sinh ra trong cùng
   một commit (để `lastmod` trong sitemap đúng, xem Phần 4).

Thêm một ngôn ngữ: thêm một dòng vào `LANDING_LANGS` (mã, `hreflang`, tên ngôn ngữ, `og:locale`) và vào `LANDING_T` (câu mô tả JSON-LD,
nhãn của thẻ Có gì mới) trong `scripts/build-site.py`, tạo `site-src/landing/<mã>.html` từ `en.html`, và thêm chuỗi của ngôn ngữ đó vào
`STR` trong `site/app.js` (mô phỏng PITR, nút sao chép, mục Tải về).

Script tự điền những phần còn lại: canonical, `hreflang`, `og:url`, `og:locale`, menu ngôn ngữ, JSON-LD (câu mô tả nằm ở `LANDING_T`
trong `scripts/build-site.py`), mục **So sánh** và thẻ **Có gì mới** (Phần 4).

**Xem thử trên máy:**

```bash
python -m http.server 8642 --directory site
```

Mở `http://localhost:8642` (tiếng Anh) và `http://localhost:8642/vi/` (tiếng Việt). Mục Tải về đọc `latest.json` thật trên
`downloads.dumpcat.cc`, nhờ dòng `localhost:8642` trong CORS. Lưu ý: khi chạy thử trên máy, `_headers` không được áp dụng.

**Deploy lại** sau khi sửa: chạy `npx wrangler deploy` ở thư mục gốc của repo. Nếu bản mới có lỗi, quay lại bản trước bằng
`npx wrangler rollback`.

**Lưu ý thêm:**

- Số phiên bản trong HTML (nút Cài đặt, các link tải, chân trang) chỉ là giá trị dự phòng, hiện khi trình duyệt tắt JavaScript,
  trong lúc trang đang tải `latest.json` hoặc khi tải lỗi. `app.js` tự thay bằng phiên bản thật. Muốn đổi giá trị dự phòng thì sửa
  `site-src/landing/index.html` và khóa `cta-install` trong `en.html`, rồi chạy lại `build-site.py`.
- Muốn đổi địa chỉ máy chủ phát hành thì sửa cả hai chỗ: `<meta name="dumpcat-release-base">` trong `site-src/landing/index.html` và
  `connect-src` trong `_headers`.
- **Sửa ảnh chia sẻ:** sửa `site/og-image.html`, render lại bằng Chrome, rồi deploy:

  ```bash
  "/c/Program Files/Google/Chrome/Application/chrome.exe" --headless=new --hide-scrollbars --force-device-scale-factor=1 \
    --window-size=1200,630 --virtual-time-budget=8000 --screenshot="$PWD/site/og.png" "file:///$PWD/site/og-image.html"
  ```

  Facebook, Zalo, X và Slack lưu cache ảnh xem trước. Để chúng lấy ảnh mới, dán link vào
  [Facebook Sharing Debugger](https://developers.facebook.com/tools/debug/) và bấm **Scrape Again**.

---

## Phần 4 — Trang so sánh và hướng dẫn cài đặt (sinh tự động)

Các trang `https://dumpcat.cc/compare/…` (tiếng Anh), `https://dumpcat.cc/vi/compare/…` (tiếng Việt), trang hướng dẫn cài đặt
`/install/` và `/vi/install/`, cùng mục **So sánh** trên landing page được tạo từ dữ liệu, không viết tay:

| File | Nội dung |
|---|---|
| `site-src/compare/<slug>.json` | Một công cụ: tên, `grid` (bảng song song: mỗi dòng là `yes` / `paid` / `partial` / `no` / `unknown` kèm ghi chú ngắn EN/VI), giá trị chi tiết từng dòng, "chọn họ nếu" / "chọn DumpCat nếu", các bước chuyển, nguồn, ngày kiểm tra |
| `site-src/install/en.html`, `vi.html` | Nội dung trang hướng dẫn cài đặt cho người mới |
| `CHANGELOG.md` | Nguồn của các trang **Có gì mới** `/changelog/` và `/changelog/<phiên bản>/` (tiếng Anh) và thẻ **Có gì mới** trên landing page (chỗ `<!-- whatsnew (scripts/build-site.py) -->` trong `site-src/landing/index.html`). Mục chưa có ngày (`## 1.7.0`, phiên bản chưa phát hành) bị bỏ qua, nên build lại site lúc nào cũng được |
| `scripts/build-site.py` | Template; các dòng của bảng song song (`GRID`) và cột DumpCat (`US_GRID`); những dòng hiện trên landing page (`LANDING_ROWS`); sinh trang so sánh và trang hướng dẫn EN/VI, hai bản của landing page (Phần 3) với mục **So sánh** của nó (chỗ `<!-- compare (scripts/build-site.py) -->`), `404.html`, `sitemap.xml` và `robots.txt` |

Sửa hoặc thêm một công cụ:

1. Sửa (hoặc chép từ file có sẵn) `site-src/compare/<slug>.json`. Mọi thông tin về sản phẩm khác **phải có nguồn** trong
   `sources`. Điều gì chưa kiểm chứng được thì ghi "Not documented on the pages we checked" chứ không đoán, và trong `grid` thì
   dùng `unknown`. Dòng nào là `paid`, `partial` hay `no` phải có ghi chú ngắn cả hai thứ tiếng. Cập nhật `checked`.
2. Chạy `python scripts/build-site.py` (chỉ dùng thư viện chuẩn của Python), xem thử bằng `python -m http.server 8642 --directory site`.
3. Deploy: `npx wrangler deploy`.

Nên kiểm tra lại giá và tính năng của các công cụ khoảng 6 tháng một lần. Sau lần deploy đầu, thêm `https://dumpcat.cc/sitemap.xml` vào
Google Search Console và Bing Webmaster Tools.

**SEO, do `build-site.py` sinh ra:**

- `sitemap.xml` liệt kê mỗi trang một lần, đúng địa chỉ canonical của nó (có `/` ở cuối). Các bản ngôn ngữ khác của một
  trang không ghi lại trong sitemap: chính trang đó đã khai báo chúng bằng `hreflang` trong `<head>`, công cụ tìm kiếm đọc
  được ở đó. (Thẻ `xhtml:link` trong sitemap làm trình duyệt hiện sitemap thành một khối chữ dính liền thay vì cây XML.)
  `lastmod` chỉ đổi khi nội dung trang đổi: lấy ngày commit cuối của file nguồn (hoặc hôm nay nếu file
  đó đang sửa dở), ngày kiểm tra của trang so sánh, ngày phát hành của trang **Có gì mới**. Với `/` và `/vi/` là ngày commit cuối của
  chính `site/index.html` / `site/vi/index.html` (và `site/app.js`), nên sửa chữ tiếng Anh chỉ đổi `lastmod` của `/`. Vì vậy hãy commit
  cả file nguồn lẫn file sinh ra trong cùng một commit.
- Chín bản của landing page trỏ `hreflang` lẫn nhau (`x-default` là `/`); các trang so sánh và hướng dẫn là cặp EN/VI. Trên các trang
  tiếng Việt, link **Trang chủ**, logo và **Tính năng** đưa về `/vi/`.
- `robots.txt` cho phép crawl mọi trang và chỉ tới sitemap. `install.sh` và `install.ps1` được gửi kèm `X-Robots-Tag: noindex`
  (trong `_headers`), nên không hiện trong kết quả tìm kiếm.
- `404.html` là trang báo không tìm thấy (`noindex`). `wrangler.jsonc` đặt `not_found_handling: "404-page"` nên mọi đường dẫn không
  tồn tại nhận trang này với mã 404.
- Bản demo (`/demo/`) được phép index và có canonical riêng; DumpCat cài trên server thì vẫn `noindex`.

---

## Phần 5 — Bản demo (dumpcat.cc/demo/)

`https://dumpcat.cc/demo/` là **giao diện thật của DumpCat** chạy trong trình duyệt với dữ liệu mẫu: không có server, không lưu gì, tải lại
trang là về như cũ. Bản demo được sinh từ `web/`, nên luôn khớp với ứng dụng:

| File | Nội dung |
|---|---|
| `site-src/demo/api.js` | Thay cho `web/assets/js/api.js`: API giả trong bộ nhớ, mô phỏng backup, verify, restore có tiến độ và log |
| `site-src/demo/data.js` | Dữ liệu mẫu: server, storage, job, lịch sử 14 ngày, binlog, restore, agent, WireGuard… (ngày tính theo lúc mở trang) |
| `site-src/demo/demo.js`, `demo.css` | Thanh "Demo" nổi (Làm lại, Cài DumpCat) và chặn các link tải file |
| `site-src/demo/types.json` | Loại storage và loại thông báo, xuất từ code Go bằng `go run ./scripts/demotypes` |
| `scripts/build-demo.py` | Chép `web/` sang `site/demo/`, thay API, đổi đường dẫn `/assets/` thành `/demo/assets/` |

Sau khi sửa `web/` hoặc `site-src/demo/`: chạy `python scripts/build-demo.py`, xem thử bằng
`python -m http.server 8642 --directory site` rồi mở `http://127.0.0.1:8642/demo/`, sau đó `npx wrangler deploy`.
Trang nào gọi một API mới mà `api.js` chưa có sẽ báo "… is not part of the demo": thêm route tương ứng vào `site-src/demo/api.js`.
