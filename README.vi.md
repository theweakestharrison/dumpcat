<p align="center">
  <img src="web/assets/favicon.svg" width="112" alt="Logo DumpCat">
</p>

<h1 align="center">DumpCat</h1>

<p align="center">
  <strong>Backup, restore và khôi phục theo thời điểm cho MySQL và MariaDB, PostgreSQL và SQL Server.</strong><br>
  Một file chạy duy nhất, tự host, có giao diện web. Không khóa database, không cần cài runtime, chạy offline.
</p>

<p align="center">
  <a href="LICENSE"><img alt="Giấy phép: Apache-2.0" src="https://img.shields.io/badge/license-Apache--2.0-blue"></a>
  <img alt="Go 1.26 trở lên" src="https://img.shields.io/badge/go-1.26%2B-00ADD8">
  <img alt="Chạy trên Linux và Windows" src="https://img.shields.io/badge/platform-Linux%20%7C%20Windows-555">
  <img alt="MySQL 5.7 đến 8.4, MariaDB 10 và 11" src="https://img.shields.io/badge/MySQL%205.7--8.4%20%C2%B7%20MariaDB%2010--11-4479A1">
</p>

<p align="center">
  <a href="https://dumpcat.cc"><b>dumpcat.cc</b></a> · <a href="https://dumpcat.cc/#download">Tải về</a> ·
  <a href="https://dumpcat.cc/demo/">Dùng thử demo</a> ·
  <a href="https://github.com/theweakestharrison/DumpCat">GitHub</a> ·
  <a href="README.md">English</a> · <b>Tiếng Việt</b>
</p>

---

DumpCat quản lý toàn bộ vòng đời backup của các server MySQL, MariaDB, PostgreSQL và SQL Server ngay trên trình duyệt. Nó lên lịch backup,
nén và mã hóa, chép bản sao lên những nơi lưu trữ bạn chọn, kiểm tra xem bản backup có đọc lại được không, và restore khi
cần, đến đúng từng giây nhờ lưu trữ binlog. DumpCat chạy trên Linux hoặc Windows ngay cạnh database, dưới dạng một file
binary Go có sẵn giao diện.

## Điểm nổi bật

- **Không khóa database.** InnoDB được backup từ một snapshot nhất quán trong khi ứng dụng vẫn đọc ghi bình thường.
- **Bốn loại database, mỗi loại một bộ công cụ riêng.** MySQL và Percona Server, MariaDB, PostgreSQL, SQL Server:
  `mysqldump`, `mydumper` chạy song song và backup vật lý nóng bằng XtraBackup hoặc MariaBackup, có cả incremental;
  `pg_dump` dump các bảng song song, và `pg_basebackup`; backup native full và differential của SQL Server, không cần
  cài công cụ client nào. Chi tiết ở [bảng bên dưới](#dumpcat-hỗ-trợ-những-gì).
- **Khôi phục theo thời điểm.** Lưu binlog hoặc WAL liên tục, hoặc log backup SQL Server theo lịch, để restore về một mốc
  thời gian hay một vị trí chính xác trong log, ví dụ ngay trước một câu `DROP TABLE` lỡ tay. Chuỗi log được kiểm tra
  trước khi restore bất cứ gì.
- **Lưu ở đâu cũng được.** Ổ local hoặc NFS, S3 và các dịch vụ tương thích (Cloudflare R2, MinIO, Wasabi…), Google Drive,
  OneDrive / SharePoint, FTP / FTPS, SFTP, WebDAV, Backblaze B2, Azure Blob, Dropbox và mọi backend của rclone. Một job có
  thể ghi ra nhiều nơi, mỗi nơi có chính sách giữ bản riêng.
- **Database nằm sau firewall.** Một agent nhỏ trên server database chủ động kết nối ra DumpCat, hoặc server đó tham gia
  WireGuard tích hợp của DumpCat (tắt mặc định), nên không cần mở port nào tới database. Backup, restore và khôi phục
  theo thời điểm đều chạy được qua cả hai cách.
- **Mã hóa và kiểm tra.** Nén zstd, mã hóa bằng [age](https://age-encryption.org) trước khi dữ liệu rời khỏi server,
  SHA-256 cho từng file, và mỗi bản backup được đọc lại ngay sau khi ghi: giải mã, giải nén, kiểm tra, nên bản hỏng lộ
  ra ngay chứ không đợi tới lúc restore.
- **Biết ngay khi backup ngừng chạy.** Mỗi job được đối chiếu với lịch chạy của chính nó: quá hạn mà chưa có bản backup
  thành công, hoặc việc lưu binlog bị chậm, DumpCat báo một lần và báo lại khi đã ổn. Báo cáo tuần qua email tóm tắt cả
  tuần, và endpoint Prometheus `/metrics` (tắt mặc định) cấp số liệu cho dashboard và luật cảnh báo của bạn.
- **Diễn tập restore.** Theo lịch, DumpCat restore bản backup mới nhất vào một server sandbox dưới tên tạm, so từng bảng
  và số dòng với những gì backup đã ghi nhận, chạy các assertion SQL của bạn, ghi lại thời gian restore (RTO) rồi xóa
  bản sao. Backup đạt được đánh dấu **Restore-tested**; diễn tập thất bại sẽ gửi cảnh báo.
- **Restore cẩn thận.** Restore sang tên database mới hoặc sang instance / phiên bản khác (tự gỡ `DEFINER`, đổi
  collation, bỏ `GTID_PURGED`), thử giải mã mọi file trước khi xóa bất cứ thứ gì, chế độ nạp nhanh, và tự rollback khi
  restore vật lý thay datadir.
- **Dùng được cho cả đội.** Bao nhiêu user cũng được, với vai trò admin, operator hoặc viewer; đăng nhập một lần bằng Google,
  Microsoft Entra ID hay mọi nhà cung cấp OpenID Connect, đăng nhập hai lớp TOTP, API token chỉ đọc, toàn quyền hoặc Manage,
  nhật ký thao tác, và thông báo qua email HTML, Telegram, Slack, Discord hoặc webhook có ký HMAC.
- **Tên và logo của bạn (white-label).** [Settings → Branding](docs/manual.vi.md#thương-hiệu-tên-và-logo) thay chữ DumpCat
  bằng tên và logo của bạn trên giao diện web, trang đăng nhập, tab trình duyệt và trong thông báo (tiêu đề email bắt đầu bằng
  `[tên của bạn]`), ví dụ khi bạn backup cho khách hàng.
- **Làm việc với trợ lý AI.** Endpoint [MCP](docs/manual.vi.md#trợ-lý-ai-mcp) cho Claude Desktop, Claude Code và các
  MCP client khác xem trạng thái, đọc backup và log, tìm nguyên nhân lỗi, mặc định chỉ đọc; với token toàn quyền,
  trợ lý còn chạy được backup, verify và diễn tập restore, và với token Manage thì thiết lập và sửa được server,
  storage, job và diễn tập. Mật khẩu, khóa và passphrase không bao giờ đi qua trợ lý.
- **Định dạng chuẩn.** Mỗi bản backup có `manifest.json` và dùng zstd / gzip / age / xbstream, nên bạn restore được chỉ
  với `age`, `zstd` và `mysql`, hoặc import catalog vào một DumpCat cài mới.
- **Luôn là bản mới nhất, cập nhật OTA.** Bản chính thức được ký và phát hành từ máy chủ build của DumpCat, nên không phải
  tự build lại. Bật tự động cài là bản mới tự về trong khung giờ bạn chọn: DumpCat chờ các job đang chạy, backup database
  của chính nó, kiểm tra chữ ký bản phát hành và rollback nếu phiên bản mới không khởi động được.
  [Cách hoạt động](docs/manual.vi.md#36-cập-nhật)

## DumpCat hỗ trợ những gì

<p align="center">
  <img src="docs/img/db/mysql.svg" height="48" alt="MySQL">&nbsp;
  <img src="docs/img/db/percona.svg" height="48" alt="Percona Server">&nbsp;
  <img src="docs/img/db/mariadb.svg" height="48" alt="MariaDB">&nbsp;
  <img src="docs/img/db/postgresql.svg" height="48" alt="PostgreSQL">&nbsp;
  <img src="docs/img/db/sqlserver.svg" height="48" alt="SQL Server">
</p>

| | MySQL · Percona · MariaDB | PostgreSQL | SQL Server |
|---|:---:|:---:|:---:|
| Phiên bản | MySQL 5.7 · 8.0 · 8.4<br>MariaDB 10 · 11 | 10 trở lên | 2012 trở lên |
| Backup logical | ✅<br><small>mysqldump</small> | ✅<br><small>pg_dump</small> | — |
| Dump và restore song song | ✅<br><small>mydumper</small> | ✅<br><small>pg_dump&nbsp;-j</small> | — |
| Backup vật lý khi database đang chạy | ✅<br><small>XtraBackup&nbsp;/&nbsp;MariaBackup</small> | ✅<br><small>pg_basebackup</small> | ✅<br><small>backup&nbsp;native</small> |
| Incremental / differential | ✅ | ❌ | ✅ |
| User, role và quyền | ✅ | ✅ | ❌ |
| Khôi phục theo thời điểm | ✅<br><small>binlog</small> | ✅<br><small>WAL</small> | ✅<br><small>log&nbsp;backup</small> |
| Restore sang tên mới hoặc server khác | ✅ | ✅ | ✅ |
| Diễn tập restore theo lịch | ✅ | ✅ | ✅ |
| Qua agent hoặc WireGuard | ✅ | ✅ | ✅ |
| DumpCat chạy trên Windows | 🟡 | ✅ | ✅ |

✅&nbsp;có · 🟡 một phần · ❌ chưa hỗ trợ · — không áp dụng

**Database nào cũng có** nén, mã hóa age và SHA-256, đọc lại kiểm tra sau mỗi bản backup, kiểm tra mọi file trước khi
restore thay dữ liệu, mọi nơi lưu trữ với retention riêng, lịch chạy, cảnh báo backup trễ, báo cáo hằng tuần,
Prometheus metrics, REST API và MCP. **DumpCat chạy trên** Linux amd64 / arm64 có systemd, và Windows Server 2016+ /
Windows 10+ 64-bit dưới dạng Windows service.

<details>
<summary><b>Ghi chú và giới hạn</b></summary>

- **Backup vật lý.** XtraBackup và MariaBackup chạy trên chính máy database: cài DumpCat ở đó, hoặc chạy agent ở đó với
  `--allow-physical`; backup logical chạy được qua mọi cách kết nối. Backup vật lý restore cả instance sang cùng phiên
  bản và chưa diễn tập được. `pg_basebackup` chỉ backup full.
- **Restore và diễn tập** sang tên mới, sang server hoặc phiên bản khác dùng backup logical (mysqldump mỗi database một
  file, mydumper, pg_dump); với MySQL và MariaDB có sửa `DEFINER`, collation và `GTID_PURGED`.
- **SQL Server** chạy trên Windows, Linux hoặc trong container; không hỗ trợ Azure SQL Database hay Managed Instance.
  SQL Server không có công cụ dump logical nên DumpCat dùng chính backup của SQL Server, được ghi ngay trên máy SQL
  Server: DumpCat đọc ở đó, qua một thư mục chia sẻ, hoặc qua agent chạy với `--allow-mssql-dir` để chuyển file về
  ([thư mục backup](docs/manual.vi.md#sql-server)). Restore sang cùng phiên bản hoặc mới hơn, diễn tập kết thúc bằng
  `DBCC CHECKDB`. Login là đối tượng của server, không nằm trong bản backup database: hãy tạo lại trên server đích (log
  restore liệt kê các user chưa có login ở đó).
- **DumpCat trên Windows.** MySQL có mysqldump, lưu binlog và khôi phục theo thời điểm; MariaDB có mariadb-dump,
  MariaBackup (chép vào datadir bằng tay) và khôi phục theo thời điểm từ binlog; mydumper và XtraBackup chỉ có cho Linux.
  PostgreSQL đủ mọi thứ, trừ việc thay data directory của cluster trên chính máy đó. Với database hàng trăm GB và backup
  vật lý MySQL, hãy chạy DumpCat trên Linux ([những gì chạy được trên Windows](docs/manual.vi.md#35-windows)).

</details>

## So sánh với các công cụ khác

DumpCat so với các công cụ backup MySQL, MariaDB khác. Mỗi dấu đều được đối chiếu với tài liệu của chính hãng vào tháng 10/2026; bản so sánh đầy đủ, kèm ghi chú và nguồn, ở [dumpcat.cc/vi/compare](https://dumpcat.cc/vi/compare/).

✅&nbsp;có · 💲 gói trả phí · 🟡 một phần · ❌ không · ❔ không có thông tin

| | **DumpCat** | [Portabase](https://dumpcat.cc/vi/compare/portabase/) | [Databasus](https://dumpcat.cc/vi/compare/databasus/) | [SQLBackupAndFTP](https://dumpcat.cc/vi/compare/sqlbackupandftp/) | [SimpleBackups](https://dumpcat.cc/vi/compare/simplebackups/) | [SqlBak](https://dumpcat.cc/vi/compare/sqlbak/) |
|---|:---:|:---:|:---:|:---:|:---:|:---:|
| Mã nguồn mở | ✅ | ✅ | ✅ | ❌ | ❌ | ❌ |
| Mọi tính năng miễn phí | ✅ | ✅ | ✅ | 🟡 | 🟡 | 🟡 |
| Chạy trên server của bạn | ✅ | ✅ | ✅ | ✅ | ❌ | 🟡 |
| Windows | ✅ | ❔ | ✅ | ✅ | 🟡 | ✅ |
| Giao diện web | ✅ | ✅ | ✅ | ❌ | ✅ | ✅ |
| Dump song song (mydumper) | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ |
| Backup vật lý nóng | ✅ | ❌ | ❌ | ❌ | 🟡 | ❌ |
| Backup incremental | ✅ | ❌ | ❌ | 💲 | ✅ | ✅ |
| Khôi phục theo thời điểm | ✅ | ❌ | ❌ | 🟡 | 🟡 | ✅ |
| Restore ngay trong công cụ | ✅ | ✅ | ✅ | ✅ | 🟡 | ✅ |
| Tự kiểm tra bản backup | ✅ | ❔ | ✅ | 🟡 | 🟡 | 🟡 |
| Nhiều bản sao cho một backup | ✅ | ✅ | ❌ | ✅ | 💲 | ✅ |
| Mã hóa | ✅ | ✅ | ✅ | 💲 | ✅ | 💲 |
| Dữ liệu không qua hãng | ✅ | ✅ | ✅ | ✅ | 🟡 | ✅ |
| Agent, không mở port vào | ✅ | ✅ | ❌ | ❌ | ✅ | ✅ |
| VPN tunnel có sẵn | ✅ | ❌ | 🟡 | 🟡 | 🟡 | 🟡 |
| Báo qua chat và webhook | ✅ | ✅ | ✅ | 💲 | 💲 | ❔ |
| MCP cho trợ lý AI | ✅ | ✅ | ❌ | ❌ | 💲 | ❌ |

Portabase và Databasus còn backup cả MongoDB, Redis và các engine khác; DumpCat chỉ tập trung vào MySQL, MariaDB, PostgreSQL và SQL Server nhưng làm sâu hơn với từng loại (xem [DumpCat hỗ trợ những gì](#dumpcat-hỗ-trợ-những-gì)).

## Cách hoạt động

```mermaid
flowchart LR
  db[("MySQL / MariaDB")]
  pipe["nén (zstd)<br/>mã hóa (age)<br/>SHA-256"]
  db -- "mysqldump · mydumper · XtraBackup / MariaBackup" --> pipe
  db -- "stream binlog" --> pipe
  pipe --> local["Ổ local / NFS"]
  pipe --> s3["S3 · R2 · MinIO · Wasabi"]
  pipe --> drive["Google Drive · OneDrive"]
  pipe --> remote["SFTP · FTP · WebDAV · rclone"]
```

Restore chạy cùng đường ống theo chiều ngược lại. Backup logical được stream thẳng vào `mysql`, không cần file tạm;
backup mydumper và backup vật lý được giải nén trước, rồi nạp bằng `myloader` hoặc prepare bằng XtraBackup / MariaBackup.

## Ba cách kết nối tới database

DumpCat tới được database theo một trong ba cách, chọn riêng cho từng server (**Database servers → Add server →
Connect**). Backup, restore và khôi phục theo thời điểm chạy như nhau với cả ba cách.

**1. Kết nối trực tiếp qua port MySQL.** DumpCat kết nối tới database như mọi client: `host:3306`, có thể bật TLS, hoặc
qua unix socket khi DumpCat chạy ngay trên server database. Dùng khi hai máy cùng mạng hoặc cùng VPC.

```mermaid
flowchart LR
  dc["DumpCat"] == "giao thức MySQL · TCP 3306<br/>do DumpCat mở" ==> fw{{"firewall<br/>mở 3306 cho DumpCat"}}
  subgraph D["Server database"]
    db[("MySQL / MariaDB<br/>nghe trên mạng")]
  end
  fw ==> db
```

**2. VPN tích hợp sẵn (WireGuard).** DumpCat tự chạy một endpoint WireGuard — không cần cài gì thêm trên server backup,
tắt cho tới khi bạn bật. Server database tham gia bằng công cụ WireGuard chuẩn và tự giữ tunnel từ phía nó, nên có thể
nằm sau NAT và không nhận kết nối vào. [Cách cài đặt](docs/manual.vi.md#38-wireguard-tích-hợp)

```mermaid
flowchart RL
  subgraph D["Server database"]
    db[("MySQL / MariaDB<br/>nghe trên 10.66.0.2")] <-- "10.66.0.2:3306" --> wg["wg-quick@dumpcat"]
  end
  wg == "WireGuard · UDP 51820<br/>do server database mở ra" ==> fw{{"firewall / NAT<br/>không mở port vào"}}
  fw ==> dc["DumpCat<br/>WireGuard tích hợp 10.66.0.1"]
```

**3. Agent.** Một `dumpcat agent` nhỏ trên server database chủ động kết nối ra DumpCat bằng HTTPS và mở các kết nối
database mà DumpCat cần — chỉ tới những địa chỉ trong danh sách cho phép của nó. MySQL giữ nguyên, kể cả khi chỉ nghe
trên 127.0.0.1. [Cách cài đặt](docs/manual.vi.md#37-server-database-không-cho-kết-nối-vào-agent)

```mermaid
flowchart RL
  subgraph D["Server database"]
    db[("MySQL / MariaDB<br/>127.0.0.1:3306")] <-- "kết nối local" --> ag["dumpcat agent"]
  end
  ag == "HTTPS · WebSocket<br/>do agent mở ra" ==> fw{{"firewall / NAT<br/>không mở port vào"}}
  fw ==> dc["DumpCat"]
```

| | 1. Trực tiếp | 2. VPN tích hợp (WireGuard) | 3. Agent |
|---|---|---|---|
| Ai mở kết nối | DumpCat → database | Server database → DumpCat | Server database → DumpCat |
| Port phải mở trên server database | MySQL (3306) cho địa chỉ của DumpCat | Không | Không |
| Server database đi ra tới DumpCat qua | — (DumpCat tự kết nối tới) | UDP 51820 | HTTPS (port giao diện web) |
| Cài trên server database | Không cần | WireGuard (`wg-quick`) và file DumpCat tạo sẵn | `dumpcat agent` (một lệnh cài) |
| Cấu hình MySQL | User backup được phép từ địa chỉ của DumpCat | Nghe trên địa chỉ VPN; user được phép từ 10.66.0.1 | Giữ nguyên |
| Backup vật lý (XtraBackup / MariaBackup) | Có, khi DumpCat chạy trên server database | Không (cần DumpCat trên server database) | Có, khi bật `--allow-physical`: agent chạy công cụ trên server database và stream bản backup về |
| File backup của SQL Server | DumpCat trên máy SQL Server, hoặc một thư mục chia sẻ | Một thư mục chia sẻ (SMB, mount) | Có, khi bật `--allow-mssql-dir`: agent chuyển file, không cần chia sẻ gì |
| Hợp với | Cùng mạng hoặc VPC, hoặc DumpCat chạy trên server database | Đội đã dùng WireGuard; một gateway dẫn tới cả mạng nội bộ | Máy sau NAT hoặc firewall chặt; không đụng tới MySQL hay mạng |
| Thiết lập ở | Database servers | Trang WireGuard, rồi Database servers | Trang Agents, rồi Database servers |

## Bắt đầu nhanh

Muốn xem trước? **[Bản demo](https://dumpcat.cc/demo/)** chạy giao diện thật với dữ liệu mẫu ngay trong trình duyệt.

### 1. Cài đặt

Mới làm quen với server? **[Hướng dẫn cài từng bước](https://dumpcat.cc/vi/install/)** đi qua mọi thứ, từ cách mở cửa
sổ dòng lệnh tới bản backup đầu tiên.

**Linux**: một lệnh trên server, bằng user có `sudo`:

```bash
curl -fsSL https://dumpcat.cc/install.sh | sudo sh
```

Lệnh này tải gói mới nhất từ `https://downloads.dumpcat.cc`, kiểm tra SHA-256 theo `SHA256SUMS` của bản phát hành rồi
chạy `install.sh` trong gói. Bộ cài đặt chép binary vào `/opt/dumpcat`, tạo service systemd `dumpcat`, và cài rclone,
MySQL / MariaDB client cùng các công cụ backup khớp với database server trên máy. Tùy chọn đặt sau `sh -s --`, ví dụ
`curl -fsSL https://dumpcat.cc/install.sh | sudo sh -s -- --port 9090`: `--port 9090`, `--listen 127.0.0.1`, `--tls-auto`,
`--postgres 16` (cài thêm client PostgreSQL 16), `--no-deps`, `--uninstall [--purge]`.
Trên server database không cho kết nối vào, cùng bộ cài này chỉ cài [agent](docs/manual.vi.md#37-server-database-không-cho-kết-nối-vào-agent):
`--agent --server https://backup.example.com --key dca_…` (trang **Agents** hiện sẵn lệnh kèm khóa).

**Windows**: tải file `.zip` trên [dumpcat.cc](https://dumpcat.cc/#download), giải nén (chuột phải, **Extract All**) rồi bấm
đúp **Install DumpCat**; Windows sẽ hỏi quyền quản trị. Hoặc, trong PowerShell chạy bằng quyền Administrator:

```powershell
irm https://dumpcat.cc/install.ps1 | iex
```

**Tự tải gói về**: phiên bản mới nhất, ghi chú phát hành và mã kiểm tra có trên [dumpcat.cc](https://dumpcat.cc/#download),
các gói nằm ở `https://downloads.dumpcat.cc/<phiên bản>/`. Gói nào cũng có `START-HERE.txt`; trên Linux, giải nén rồi chạy
`sudo ./install.sh`. Kiểm tra file tải về bằng `SHA256SUMS` của bản phát hành (`sha256sum -c --ignore-missing SHA256SUMS`,
hoặc `Get-FileHash` trên Windows). Muốn tự build gói cài đặt, xem [Build từ mã nguồn](#build-từ-mã-nguồn).

### 2. Chạy bản backup đầu tiên

1. Mở `http://<ip-server>:6369` và tạo tài khoản admin. Trang thiết lập sẽ hỏi **mã thiết lập** (setup token) dùng một lần
   mà bộ cài đặt đã in ra, để người khác truy cập được cổng này không thể giành tài khoản admin trước bạn. Muốn xem lại mã,
   chạy `sudo dumpcat setup-token` (Windows, quyền Administrator: `& "C:\Program Files\DumpCat\dumpcat.exe" setup-token`)
   hoặc xem log của service. (Cổng mặc định bấm trên bàn phím điện thoại sẽ ra chữ *MEOW*.)
2. Tạo một user MySQL có quyền backup chỉ đọc ([các lệnh GRANT](docs/manual.vi.md#4-bắt-đầu-sử-dụng)).
3. **Database servers → Add server → Test connection.** Chọn cách DumpCat kết nối: trực tiếp, qua WireGuard hoặc qua
   agent ([ba cách kết nối](#ba-cách-kết-nối-tới-database)). DumpCat hiển thị version, dung lượng từng database, các quyền còn thiếu và phương thức backup nào dùng được.
4. **Storage → Add storage → Test.**
5. **Backup jobs → New job.** Chọn server, phương thức, database, lịch chạy, mã hóa, nơi lưu và retention, rồi bấm
   **Save & run now**. Mỗi bản backup được kiểm tra ngay sau khi ghi (mặc định kiểm một bản).

Khi chạy thật, hãy đặt DumpCat sau HTTPS bằng [cấu hình nginx mẫu hoặc TLS có sẵn](docs/manual.vi.md#34-đặt-sau-nginx--https-khuyến-nghị-cho-môi-trường-thật)
và giới hạn ai được truy cập. Muốn DumpCat luôn là bản mới nhất mà không phải làm gì, hãy tích **Settings → Updates →
Install new versions automatically**.

## Chọn phương thức backup

| | mysqldump | mydumper | XtraBackup / MariaBackup |
|---|---|---|---|
| Khóa InnoDB | Không | Vài mili giây | Không (một backup lock rất ngắn ở cuối) |
| Tốc độ restore | Chậm nhất | Nhanh, song song | Rất nhanh: copy file + prepare |
| Incremental | Không | Không | Có |
| Restore một database hoặc đổi tên | Có | Có | Không, cả instance |
| Hợp với | Dưới 20–50 GB | Database lớn cần restore từng DB | 100 GB đến vài TB, cần RTO thấp |

Server PostgreSQL được backup bằng `pg_dump` (định dạng thư mục, các bảng song song, không khóa ghi) hoặc
`pg_basebackup` (cả cluster, làm nền cho việc lưu WAL và khôi phục theo thời điểm): xem
[PostgreSQL](docs/manual.vi.md#postgresql). Database SQL Server được backup bằng chính backup full và differential của
SQL Server (cùng log backup cho khôi phục theo thời điểm), ghi vào một thư mục backup mà DumpCat đọc: trên máy SQL
Server, một share cả hai máy cùng thấy, hoặc thư mục ngay trên máy SQL Server mà agent chuyển file đi — xem
[SQL Server](docs/manual.vi.md#sql-server).

Chi tiết và khuyến nghị: [Chọn phương thức backup](docs/manual.vi.md#2-chọn-phương-thức-backup).

## Tài liệu

Hướng dẫn đầy đủ nằm ở [docs/manual.vi.md](docs/manual.vi.md) ([English](docs/manual.md)) và có sẵn trong mọi gói cài đặt.

| Chủ đề | |
|---|---|
| Cài trên Linux và Windows, HTTPS | [Cài đặt](docs/manual.vi.md#3-cài-đặt) |
| Server database không cho kết nối vào | [Agent](docs/manual.vi.md#37-server-database-không-cho-kết-nối-vào-agent) · [WireGuard](docs/manual.vi.md#38-wireguard-tích-hợp) |
| S3, Google Drive, OneDrive, FTP, SFTP… | [Cấu hình nơi lưu trữ](docs/manual.vi.md#5-cấu-hình-nơi-lưu-trữ) |
| Lịch chạy, incremental, retention | [Job, lịch chạy, retention](docs/manual.vi.md#6-job-lịch-chạy-retention) |
| Restore logical và vật lý | [Restore](docs/manual.vi.md#7-restore) |
| Lưu binlog, khôi phục về một thời điểm | [PITR](docs/manual.vi.md#8-lưu-trữ-log--khôi-phục-theo-thời-điểm-pitr) |
| Restore không cần DumpCat, cài lại DumpCat | [Khôi phục thủ công](docs/manual.vi.md#9-khôi-phục-thủ-công-khi-không-có-dumpcat) · [Khôi phục thảm họa](docs/manual.vi.md#10-khôi-phục-thảm-họa) |
| Phân quyền, 2FA, mã hóa dữ liệu lưu trữ | [Bảo mật](docs/manual.vi.md#11-bảo-mật) |
| REST API, webhook, trợ lý AI (MCP) | [API & tự động hóa](docs/manual.vi.md#12-api--tự-động-hóa) · [MCP](docs/manual.vi.md#trợ-lý-ai-mcp) |
| Biến môi trường, dòng lệnh | [Cấu hình hệ thống](docs/manual.vi.md#13-cấu-hình-hệ-thống) |
| Cập nhật, release server, ký bản phát hành | [Cập nhật](docs/manual.vi.md#36-cập-nhật) |
| Lỗi thường gặp | [Xử lý sự cố](docs/manual.vi.md#14-xử-lý-sự-cố) |

## Build từ mã nguồn

Cần Go 1.26 trở lên. Không cần trình biên dịch C: binary được build với `CGO_ENABLED=0` và SQLite viết thuần Go.

```bash
go vet ./...
go test ./...
go build -o dumpcat ./cmd/dumpcat
```

Chạy thử trên máy, lấy giao diện trực tiếp từ thư mục để sửa UI không cần build lại:

```bash
DUMPCAT_DATA_DIR=./data DUMPCAT_LISTEN=127.0.0.1:6369 DUMPCAT_WEB_DIR=./web ./dumpcat serve
```

Build các gói phát hành (trên Windows: `.\build.ps1`):

```bash
./build.sh
```

Lệnh này tạo binary Linux amd64 / arm64 và Windows amd64, `dumpcat-<version>-linux.tar.gz`,
`dumpcat-<version>-windows-amd64.zip` và nguồn cập nhật trong `dist/update/`. Số phiên bản lấy từ `VERSION`, ghi chú phát
hành lấy từ `CHANGELOG.md`. Cách phát hành bản cập nhật và ký bản phát hành nằm ở [Cập nhật](docs/manual.vi.md#36-cập-nhật).

```
cmd/dumpcat    dòng lệnh: serve, agent, user, decode, update, release
internal/      API, bộ chạy backup, các engine, lưu trữ, đường ống nén và mã hóa, SQLite, tunnel agent và WireGuard,
               thông báo, bộ cập nhật…
web/           giao diện bằng HTML, CSS và JavaScript thuần, nhúng vào binary
scripts/       bộ cài cho Linux và Windows (cả cho agent), script upload bản phát hành (publish-release.sh),
               script sinh website (build-site.py, build-demo.py)
deploy/        systemd unit cho DumpCat và agent, file env mẫu, cấu hình nginx
site/          dumpcat.cc: landing page, trang so sánh và bản demo (site/demo, sinh từ web/), chạy bằng Cloudflare Worker
               khai báo trong wrangler.jsonc
site-src/      nguồn của các trang được sinh ra: dữ liệu so sánh, API giả và dữ liệu mẫu của bản demo
docs/          hướng dẫn tiếng Anh và tiếng Việt, cách deploy website (website.vi.md)
```

## Đóng góp

Mọi báo lỗi, bản sửa, tài liệu và bản dịch đều được hoan nghênh. Vui lòng đọc [CONTRIBUTING.md](CONTRIBUTING.md) (tiếng
Anh) trước. Lỗ hổng bảo mật xin báo riêng theo hướng dẫn trong [SECURITY.md](SECURITY.md).

## Giấy phép

DumpCat phát hành theo [Apache License 2.0](LICENSE).
Copyright 2026 [theweakestharrison](https://github.com/theweakestharrison); xem [NOTICE](NOTICE).

Các file chạy có chứa những module Go mang giấy phép riêng, được liệt kê kèm toàn văn giấy phép trong
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md); driver github.com/go-sql-driver/mysql (MPL-2.0) được dùng nguyên bản,
không sửa đổi, và mã nguồn của nó có sẵn tại upstream: https://github.com/go-sql-driver/mysql.

DumpCat gọi các công cụ bên ngoài như mysqldump, mydumper, Percona XtraBackup, MariaBackup và rclone. Các công cụ này được
cài riêng và giữ giấy phép của chúng.
