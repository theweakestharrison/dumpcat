(function () {
  "use strict";

  var EN = {
    "nav-methods": "Methods",
    "nav-storage": "Storage",
    "nav-connect": "Connectivity",
    "nav-demo": "Demo",
    "nav-ops": "Operations",
    "nav-download": "Download",
    "nav-install": "Install",
    "nav-compare": "Compare",
    "eyebrow": "Open source · Backup &amp; restore for MySQL and MariaDB",
    "h1": "Backups that never lock the database. <span class=\"soft\">Recovery to the exact second.</span>",
    "lead": "DumpCat is a single binary with a web UI. It schedules backups, compresses and encrypts them, ships copies to S3, Google Drive, OneDrive or SFTP, verifies them and brings your data back when you need it. Runs on Linux and Windows, fully offline.",
    "cta-install": "Install <span data-version>1.4.0</span>",
    "cta-demo": "Try the demo",
    "cta-github": "Source on GitHub",
    "pitr-title": "Point-in-time recovery",
    "pitr-tag": "Simulation",
    "pitr-sub": "Someone dropped the <code>orders</code> table at 14:37:12. Drag the marker to pick a recovery point: DumpCat picks the newest base backup and the binlog files to replay.",
    "tl-now": "now",
    "lg-backup": "Full backup (mysqldump)",
    "lg-binlog": "Archived binlog, at most 15 minutes per file",
    "lg-replay": "Replayed on restore",
    "lg-incident": "Incident",
    "ro-to": "Recover to",
    "ro-base": "Base backup",
    "ro-replay": "Replay binlogs",
    "tl-snap": "Jump to just before the incident",
    "m-label": "Backup methods",
    "m-title": "Three tools, picked by database size",
    "m-intro": "DumpCat drives <code>mysqldump</code>, <code>mydumper</code> and XtraBackup / MariaBackup. When you add a server, <em>Test connection</em> shows the version, datadir, the size of each database and which methods work on that machine.",
    "m-table": "<thead><tr><th scope=\"col\"></th><th scope=\"col\">mysqldump<small>logical</small></th><th scope=\"col\">mydumper<small>logical, parallel</small></th><th scope=\"col\">XtraBackup / MariaBackup<small>physical, hot</small></th></tr></thead><tbody>" +
      "<tr><th scope=\"row\">Locks the database?</th><td data-m=\"mysqldump\">Not for InnoDB, thanks to <code>--single-transaction</code></td><td data-m=\"mydumper\">A global lock for a few milliseconds to sync threads, then none</td><td data-m=\"physical\">No InnoDB locking; a very brief backup lock at the end</td></tr>" +
      "<tr><th scope=\"row\">Backup speed</th><td data-m=\"mysqldump\">1 thread per database, several databases in parallel</td><td data-m=\"mydumper\">Multi-threaded, tables chunked by rows</td><td data-m=\"physical\">Fastest: multi-threaded file copy</td></tr>" +
      "<tr><th scope=\"row\">Restore speed</th><td data-m=\"mysqldump\">Slowest</td><td data-m=\"mydumper\">Fast: parallel, indexes built after load</td><td data-m=\"physical\">Very fast: file copy + prepare</td></tr>" +
      "<tr><th scope=\"row\">Incremental</th><td class=\"no\" data-m=\"mysqldump\">No</td><td class=\"no\" data-m=\"mydumper\">No</td><td class=\"yes\" data-m=\"physical\">Yes</td></tr>" +
      "<tr><th scope=\"row\">Restore one database, rename</th><td class=\"yes\" data-m=\"mysqldump\">Yes</td><td class=\"yes\" data-m=\"mydumper\">Yes</td><td class=\"no\" data-m=\"physical\">No, whole instance</td></tr>" +
      "<tr class=\"fit\"><th scope=\"row\">Best for</th><td data-m=\"mysqldump\">Databases under 20–50 GB</td><td data-m=\"mydumper\">Large databases restored one at a time</td><td data-m=\"physical\">100 GB to several TB, low RTO</td></tr></tbody>",
    "s-label": "Storage",
    "s-title": "One job, copies in several places",
    "s-intro": "Write to local disk and then upload, or stream straight to the cloud with no temporary disk. Each storage keeps its own number of copies, for example 3 backups locally and 30 days on S3.",
    "s-targets": "<li><b>Local / NFS</b><span>Fastest to restore from; keep it off the datadir disk</span></li>" +
      "<li><b>S3</b><span>AWS, Cloudflare R2, MinIO, Wasabi, DigitalOcean</span></li>" +
      "<li><b>Google Drive</b><span>OAuth or a Service Account</span></li>" +
      "<li><b>OneDrive</b><span>SharePoint included</span></li>" +
      "<li><b>FTP / FTPS</b><span>Explicit or Implicit TLS</span></li>" +
      "<li><b>SFTP</b><span>Password or private key</span></li>" +
      "<li><b>WebDAV</b><span>Nextcloud and friends</span></li>" +
      "<li><b>Backblaze B2</b><span>Plus Azure Blob and Dropbox</span></li>" +
      "<li><b>rclone</b><span>70+ more backends</span></li>",
    "s-aside-title": "Standard formats, no lock-in",
    "s-aside-text": "Lost the machine running DumpCat? Install a fresh copy, add the storage again and click <em>Import backup catalog</em>: every backup is listed again. Without DumpCat you can still restore with <code>age</code>, <code>zstd</code> and <code>mysql</code>.",
    "c-label": "Connectivity",
    "c-title": "Database with no open port? Still backed up.",
    "c-intro": "When the backup server cannot connect to MySQL directly, because of a firewall, NAT or MySQL listening on 127.0.0.1 only, let the database host connect out to DumpCat instead. There are two ways, and neither opens a port on the database host. Backups, restores and point-in-time recovery work as with a direct connection.",
    "c-agent-title": "Agent",
    "c-agent-fig": "<svg class=\"tp tp-h\" viewBox=\"0 0 760 300\" role=\"img\" aria-label=\"Agent: the database host opens the connection to DumpCat through its firewall\"><defs><pattern id=\"br-agent-h\" width=\"24\" height=\"14\" patternUnits=\"userSpaceOnUse\"><rect class=\"tp-brick-bg\" width=\"24\" height=\"14\"/><path class=\"tp-brick-line\" d=\"M0 .7H24M0 7.7H24M6 .7V7.7M18 7.7V14\"/></pattern></defs><rect class=\"tp-panel\" x=\"12\" y=\"24\" width=\"228\" height=\"252\" rx=\"18\"/><use href=\"#dc-logo\" x=\"28\" y=\"40\" width=\"34\" height=\"34\"/><text class=\"tp-title\" x=\"74\" y=\"55\">DumpCat</text><text class=\"tp-sub\" x=\"74\" y=\"71\">Backup server</text><rect class=\"tp-chip\" x=\"28\" y=\"92\" width=\"196\" height=\"54\" rx=\"12\"/><rect class=\"tp-ico-bg\" x=\"38\" y=\"104\" width=\"30\" height=\"30\" rx=\"8\"/><svg class=\"tp-ico\" x=\"44\" y=\"110\" width=\"18\" height=\"18\" viewBox=\"0 0 24 24\" fill=\"none\" stroke=\"currentColor\" stroke-width=\"1.8\" stroke-linecap=\"round\" stroke-linejoin=\"round\" aria-hidden=\"true\"><rect x=\"3\" y=\"4\" width=\"18\" height=\"5\" rx=\"1\"/><path d=\"M5 9v10a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1V9M10 13h4\"/></svg><text class=\"tp-chip-t\" x=\"78\" y=\"115\">Backup &amp; restore</text><text class=\"tp-chip-s\" x=\"78\" y=\"132\">mysqldump · mydumper</text><path class=\"tp-conn\" d=\"M126 146V168\"/><text class=\"tp-mono\" x=\"134\" y=\"161\">127.0.0.1:port</text><rect class=\"tp-chip accent\" x=\"28\" y=\"168\" width=\"196\" height=\"54\" rx=\"12\"/><rect class=\"tp-ico-bg\" x=\"38\" y=\"180\" width=\"30\" height=\"30\" rx=\"8\"/><svg class=\"tp-ico\" x=\"44\" y=\"186\" width=\"18\" height=\"18\" viewBox=\"0 0 24 24\" fill=\"none\" stroke=\"currentColor\" stroke-width=\"1.8\" stroke-linecap=\"round\" stroke-linejoin=\"round\" aria-hidden=\"true\"><path d=\"M10 14a5 5 0 0 0 7 0l3-3a5 5 0 0 0-7-7l-1 1\"/><path d=\"M14 10a5 5 0 0 0-7 0l-3 3a5 5 0 0 0 7 7l1-1\"/></svg><text class=\"tp-chip-t\" x=\"78\" y=\"191\">Agent endpoint</text><text class=\"tp-chip-s\" x=\"78\" y=\"208\">HTTPS · web UI port</text><circle class=\"tp-ok-dot\" cx=\"33\" cy=\"252\" r=\"3.5\"/><text class=\"tp-foot\" x=\"43\" y=\"256\">Reachable over HTTPS</text><rect class=\"tp-panel\" x=\"520\" y=\"24\" width=\"228\" height=\"252\" rx=\"18\"/><rect class=\"tp-ico-bg big\" x=\"536\" y=\"40\" width=\"34\" height=\"34\" rx=\"9\"/><svg class=\"tp-ico\" x=\"544\" y=\"48\" width=\"18\" height=\"18\" viewBox=\"0 0 24 24\" fill=\"none\" stroke=\"currentColor\" stroke-width=\"1.8\" stroke-linecap=\"round\" stroke-linejoin=\"round\" aria-hidden=\"true\"><rect x=\"3\" y=\"4\" width=\"18\" height=\"7\" rx=\"2\"/><rect x=\"3\" y=\"13\" width=\"18\" height=\"7\" rx=\"2\"/><path d=\"M7 7.5h.01M7 16.5h.01\"/></svg><text class=\"tp-title\" x=\"582\" y=\"55\">Database host</text><text class=\"tp-sub\" x=\"582\" y=\"71\">No inbound port</text><rect class=\"tp-chip\" x=\"536\" y=\"92\" width=\"196\" height=\"54\" rx=\"12\"/><g class=\"tp-cyl\"><path d=\"M548 107v22c0 3.3 5.8 6 13 6s13-2.7 13-6v-22\"/><ellipse cx=\"561\" cy=\"107\" rx=\"13\" ry=\"5\"/><path d=\"M548 118c0 3.3 5.8 6 13 6s13-2.7 13-6\"/></g><text class=\"tp-chip-t\" x=\"586\" y=\"115\">MySQL / MariaDB</text><text class=\"tp-chip-s\" x=\"586\" y=\"132\">Unchanged · 127.0.0.1 only</text><path class=\"tp-conn\" d=\"M634 146V168\"/><text class=\"tp-mono\" x=\"642\" y=\"161\">127.0.0.1:3306</text><rect class=\"tp-chip accent\" x=\"536\" y=\"168\" width=\"196\" height=\"54\" rx=\"12\"/><rect class=\"tp-ico-bg\" x=\"546\" y=\"180\" width=\"30\" height=\"30\" rx=\"8\"/><svg class=\"tp-ico\" x=\"552\" y=\"186\" width=\"18\" height=\"18\" viewBox=\"0 0 24 24\" fill=\"none\" stroke=\"currentColor\" stroke-width=\"1.8\" stroke-linecap=\"round\" stroke-linejoin=\"round\" aria-hidden=\"true\"><path d=\"M10 14a5 5 0 0 0 7 0l3-3a5 5 0 0 0-7-7l-1 1\"/><path d=\"M14 10a5 5 0 0 0-7 0l-3 3a5 5 0 0 0 7 7l1-1\"/></svg><text class=\"tp-chip-t\" x=\"586\" y=\"191\">dumpcat agent</text><text class=\"tp-chip-s\" x=\"586\" y=\"208\">Allow list 127.0.0.1:3306</text><svg class=\"tp-ico\" x=\"536\" y=\"245\" width=\"14\" height=\"14\" viewBox=\"0 0 24 24\" fill=\"none\" stroke=\"currentColor\" stroke-width=\"1.8\" stroke-linecap=\"round\" stroke-linejoin=\"round\" aria-hidden=\"true\"><rect x=\"4\" y=\"10\" width=\"16\" height=\"11\" rx=\"2\"/><path d=\"M8 10V7a4 4 0 0 1 8 0v3\"/></svg><text class=\"tp-foot\" x=\"556\" y=\"256\">Nothing opened on this host</text><text class=\"tp-wall-t\" x=\"380\" y=\"30\" text-anchor=\"middle\">Firewall / NAT</text><rect class=\"tp-wall\" x=\"362\" y=\"38\" width=\"36\" height=\"226\" rx=\"6\" fill=\"url(#br-agent-h)\"/><rect class=\"tp-pipe\" x=\"222\" y=\"185\" width=\"316\" height=\"20\" rx=\"10\"/><rect class=\"tp-rim\" x=\"357\" y=\"180\" width=\"7\" height=\"30\" rx=\"3.5\"/><rect class=\"tp-rim\" x=\"396\" y=\"180\" width=\"7\" height=\"30\" rx=\"3.5\"/><g class=\"tp-dots\"><circle class=\"tp-dot out\" r=\"4\" cx=\"530\" cy=\"195\" opacity=\"0\"><set attributeName=\"opacity\" to=\"1\" begin=\"0.0s\"/><animateMotion dur=\"2.7s\" begin=\"0.0s\" repeatCount=\"indefinite\" path=\"M0 0L-300 0\"/></circle><circle class=\"tp-dot in\" r=\"3\" cx=\"230\" cy=\"195\" opacity=\"0\"><set attributeName=\"opacity\" to=\"1\" begin=\"0.45s\"/><animateMotion dur=\"2.7s\" begin=\"0.45s\" repeatCount=\"indefinite\" path=\"M0 0L300 0\"/></circle><circle class=\"tp-dot out\" r=\"4\" cx=\"530\" cy=\"195\" opacity=\"0\"><set attributeName=\"opacity\" to=\"1\" begin=\"0.9s\"/><animateMotion dur=\"2.7s\" begin=\"0.9s\" repeatCount=\"indefinite\" path=\"M0 0L-300 0\"/></circle><circle class=\"tp-dot in\" r=\"3\" cx=\"230\" cy=\"195\" opacity=\"0\"><set attributeName=\"opacity\" to=\"1\" begin=\"1.35s\"/><animateMotion dur=\"2.7s\" begin=\"1.35s\" repeatCount=\"indefinite\" path=\"M0 0L300 0\"/></circle><circle class=\"tp-dot out\" r=\"4\" cx=\"530\" cy=\"195\" opacity=\"0\"><set attributeName=\"opacity\" to=\"1\" begin=\"1.8s\"/><animateMotion dur=\"2.7s\" begin=\"1.8s\" repeatCount=\"indefinite\" path=\"M0 0L-300 0\"/></circle><circle class=\"tp-dot in\" r=\"3\" cx=\"230\" cy=\"195\" opacity=\"0\"><set attributeName=\"opacity\" to=\"1\" begin=\"2.25s\"/><animateMotion dur=\"2.7s\" begin=\"2.25s\" repeatCount=\"indefinite\" path=\"M0 0L300 0\"/></circle></g><rect class=\"tp-pill\" x=\"244\" y=\"148\" width=\"272\" height=\"24\" rx=\"12\"/><text class=\"tp-pill-t\" x=\"380\" y=\"164\" text-anchor=\"middle\">← HTTPS · opened by the database host</text><path class=\"tp-block\" d=\"M240 244H346\"/><circle class=\"tp-x-bg\" cx=\"356\" cy=\"244\" r=\"10\"/><path class=\"tp-x\" d=\"M352 240l8 8M360 240l-8 8\"/><text class=\"tp-block-t\" x=\"293\" y=\"236\" text-anchor=\"middle\">3306 inbound</text><text class=\"tp-block-s\" x=\"293\" y=\"262\" text-anchor=\"middle\">blocked, not needed</text></svg><svg class=\"tp tp-v\" viewBox=\"0 0 340 640\" role=\"img\" aria-label=\"Agent: the database host opens the connection to DumpCat through its firewall\"><defs><pattern id=\"br-agent-v\" width=\"24\" height=\"14\" patternUnits=\"userSpaceOnUse\"><rect class=\"tp-brick-bg\" width=\"24\" height=\"14\"/><path class=\"tp-brick-line\" d=\"M0 .7H24M0 7.7H24M6 .7V7.7M18 7.7V14\"/></pattern></defs><rect class=\"tp-panel\" x=\"10\" y=\"10\" width=\"320\" height=\"240\" rx=\"18\"/><use href=\"#dc-logo\" x=\"26\" y=\"26\" width=\"34\" height=\"34\"/><text class=\"tp-title\" x=\"72\" y=\"41\">DumpCat</text><text class=\"tp-sub\" x=\"72\" y=\"57\">Backup server</text><rect class=\"tp-chip\" x=\"26\" y=\"78\" width=\"288\" height=\"54\" rx=\"12\"/><rect class=\"tp-ico-bg\" x=\"36\" y=\"90\" width=\"30\" height=\"30\" rx=\"8\"/><svg class=\"tp-ico\" x=\"42\" y=\"96\" width=\"18\" height=\"18\" viewBox=\"0 0 24 24\" fill=\"none\" stroke=\"currentColor\" stroke-width=\"1.8\" stroke-linecap=\"round\" stroke-linejoin=\"round\" aria-hidden=\"true\"><rect x=\"3\" y=\"4\" width=\"18\" height=\"5\" rx=\"1\"/><path d=\"M5 9v10a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1V9M10 13h4\"/></svg><text class=\"tp-chip-t\" x=\"76\" y=\"101\">Backup &amp; restore</text><text class=\"tp-chip-s\" x=\"76\" y=\"118\">mysqldump · mydumper</text><path class=\"tp-conn\" d=\"M120 132V154\"/><text class=\"tp-mono\" x=\"128\" y=\"147\">127.0.0.1:port</text><rect class=\"tp-chip accent\" x=\"26\" y=\"154\" width=\"288\" height=\"54\" rx=\"12\"/><rect class=\"tp-ico-bg\" x=\"36\" y=\"166\" width=\"30\" height=\"30\" rx=\"8\"/><svg class=\"tp-ico\" x=\"42\" y=\"172\" width=\"18\" height=\"18\" viewBox=\"0 0 24 24\" fill=\"none\" stroke=\"currentColor\" stroke-width=\"1.8\" stroke-linecap=\"round\" stroke-linejoin=\"round\" aria-hidden=\"true\"><path d=\"M10 14a5 5 0 0 0 7 0l3-3a5 5 0 0 0-7-7l-1 1\"/><path d=\"M14 10a5 5 0 0 0-7 0l-3 3a5 5 0 0 0 7 7l1-1\"/></svg><text class=\"tp-chip-t\" x=\"76\" y=\"177\">Agent endpoint</text><text class=\"tp-chip-s\" x=\"76\" y=\"194\">HTTPS · web UI port</text><circle class=\"tp-ok-dot\" cx=\"31\" cy=\"232\" r=\"3.5\"/><text class=\"tp-foot\" x=\"41\" y=\"236\">Reachable over HTTPS</text><rect class=\"tp-wall\" x=\"20\" y=\"298\" width=\"300\" height=\"32\" rx=\"6\" fill=\"url(#br-agent-v)\"/><text class=\"tp-wall-t\" x=\"318\" y=\"350\" text-anchor=\"end\">Firewall / NAT</text><rect class=\"tp-pipe\" x=\"160\" y=\"204\" width=\"20\" height=\"248\" rx=\"10\"/><rect class=\"tp-rim\" x=\"155\" y=\"293\" width=\"30\" height=\"7\" rx=\"3.5\"/><rect class=\"tp-rim\" x=\"155\" y=\"328\" width=\"30\" height=\"7\" rx=\"3.5\"/><g class=\"tp-dots\"><circle class=\"tp-dot out\" r=\"4\" cx=\"170\" cy=\"446\" opacity=\"0\"><set attributeName=\"opacity\" to=\"1\" begin=\"0.0s\"/><animateMotion dur=\"2.7s\" begin=\"0.0s\" repeatCount=\"indefinite\" path=\"M0 0L0 -234\"/></circle><circle class=\"tp-dot in\" r=\"3\" cx=\"170\" cy=\"212\" opacity=\"0\"><set attributeName=\"opacity\" to=\"1\" begin=\"0.45s\"/><animateMotion dur=\"2.7s\" begin=\"0.45s\" repeatCount=\"indefinite\" path=\"M0 0L0 234\"/></circle><circle class=\"tp-dot out\" r=\"4\" cx=\"170\" cy=\"446\" opacity=\"0\"><set attributeName=\"opacity\" to=\"1\" begin=\"0.9s\"/><animateMotion dur=\"2.7s\" begin=\"0.9s\" repeatCount=\"indefinite\" path=\"M0 0L0 -234\"/></circle><circle class=\"tp-dot in\" r=\"3\" cx=\"170\" cy=\"212\" opacity=\"0\"><set attributeName=\"opacity\" to=\"1\" begin=\"1.35s\"/><animateMotion dur=\"2.7s\" begin=\"1.35s\" repeatCount=\"indefinite\" path=\"M0 0L0 234\"/></circle><circle class=\"tp-dot out\" r=\"4\" cx=\"170\" cy=\"446\" opacity=\"0\"><set attributeName=\"opacity\" to=\"1\" begin=\"1.8s\"/><animateMotion dur=\"2.7s\" begin=\"1.8s\" repeatCount=\"indefinite\" path=\"M0 0L0 -234\"/></circle><circle class=\"tp-dot in\" r=\"3\" cx=\"170\" cy=\"212\" opacity=\"0\"><set attributeName=\"opacity\" to=\"1\" begin=\"2.25s\"/><animateMotion dur=\"2.7s\" begin=\"2.25s\" repeatCount=\"indefinite\" path=\"M0 0L0 234\"/></circle></g><rect class=\"tp-pill\" x=\"62\" y=\"256\" width=\"216\" height=\"34\" rx=\"12\"/><text class=\"tp-pill-t\" x=\"170\" y=\"270\" text-anchor=\"middle\">↑ HTTPS</text><text class=\"tp-pill-s\" x=\"170\" y=\"284\" text-anchor=\"middle\">opened by the database host</text><path class=\"tp-block\" d=\"M40 250V288\"/><circle class=\"tp-x-bg\" cx=\"40\" cy=\"298\" r=\"10\"/><path class=\"tp-x\" d=\"M36 294l8 8M44 294l-8 8\"/><text class=\"tp-block-t\" x=\"22\" y=\"352\">3306 inbound</text><text class=\"tp-block-s\" x=\"22\" y=\"367\">blocked, not needed</text><rect class=\"tp-panel\" x=\"10\" y=\"380\" width=\"320\" height=\"250\" rx=\"18\"/><rect class=\"tp-ico-bg big\" x=\"26\" y=\"396\" width=\"34\" height=\"34\" rx=\"9\"/><svg class=\"tp-ico\" x=\"34\" y=\"404\" width=\"18\" height=\"18\" viewBox=\"0 0 24 24\" fill=\"none\" stroke=\"currentColor\" stroke-width=\"1.8\" stroke-linecap=\"round\" stroke-linejoin=\"round\" aria-hidden=\"true\"><rect x=\"3\" y=\"4\" width=\"18\" height=\"7\" rx=\"2\"/><rect x=\"3\" y=\"13\" width=\"18\" height=\"7\" rx=\"2\"/><path d=\"M7 7.5h.01M7 16.5h.01\"/></svg><text class=\"tp-title\" x=\"72\" y=\"411\">Database host</text><text class=\"tp-sub\" x=\"72\" y=\"427\">No inbound port</text><rect class=\"tp-chip accent\" x=\"26\" y=\"448\" width=\"288\" height=\"54\" rx=\"12\"/><rect class=\"tp-ico-bg\" x=\"36\" y=\"460\" width=\"30\" height=\"30\" rx=\"8\"/><svg class=\"tp-ico\" x=\"42\" y=\"466\" width=\"18\" height=\"18\" viewBox=\"0 0 24 24\" fill=\"none\" stroke=\"currentColor\" stroke-width=\"1.8\" stroke-linecap=\"round\" stroke-linejoin=\"round\" aria-hidden=\"true\"><path d=\"M10 14a5 5 0 0 0 7 0l3-3a5 5 0 0 0-7-7l-1 1\"/><path d=\"M14 10a5 5 0 0 0-7 0l-3 3a5 5 0 0 0 7 7l1-1\"/></svg><text class=\"tp-chip-t\" x=\"76\" y=\"471\">dumpcat agent</text><text class=\"tp-chip-s\" x=\"76\" y=\"488\">Allow list 127.0.0.1:3306</text><path class=\"tp-conn\" d=\"M120 502V524\"/><text class=\"tp-mono\" x=\"128\" y=\"517\">127.0.0.1:3306</text><rect class=\"tp-chip\" x=\"26\" y=\"524\" width=\"288\" height=\"54\" rx=\"12\"/><g class=\"tp-cyl\"><path d=\"M38 539v22c0 3.3 5.8 6 13 6s13-2.7 13-6v-22\"/><ellipse cx=\"51\" cy=\"539\" rx=\"13\" ry=\"5\"/><path d=\"M38 550c0 3.3 5.8 6 13 6s13-2.7 13-6\"/></g><text class=\"tp-chip-t\" x=\"76\" y=\"547\">MySQL / MariaDB</text><text class=\"tp-chip-s\" x=\"76\" y=\"564\">Unchanged · 127.0.0.1 only</text><svg class=\"tp-ico\" x=\"26\" y=\"599\" width=\"14\" height=\"14\" viewBox=\"0 0 24 24\" fill=\"none\" stroke=\"currentColor\" stroke-width=\"1.8\" stroke-linecap=\"round\" stroke-linejoin=\"round\" aria-hidden=\"true\"><rect x=\"4\" y=\"10\" width=\"16\" height=\"11\" rx=\"2\"/><path d=\"M8 10V7a4 4 0 0 1 8 0v3\"/></svg><text class=\"tp-foot\" x=\"46\" y=\"610\">Nothing opened on this host</text></svg><figcaption class=\"tp-legend\"><span><i class=\"out\"></i>The database host opens the tunnel</span><span><i class=\"in\"></i>DumpCat&#x27;s database traffic flows back inside it</span></figcaption>",
    "c-agent-facts": "<div><dt>The database host needs</dt><dd>Outbound HTTPS to DumpCat</dd></div><div><dt>MySQL</dt><dd>Unchanged, even if it listens on 127.0.0.1 only</dd></div><div><dt>Install</dt><dd><code>sudo ./install.sh --agent --server … --key …</code></dd></div><div><dt>Good for</dt><dd>Leaving the network and the MySQL configuration alone</dd></div>",
    "c-wg-title": "Built-in WireGuard",
    "c-wg-tag": "UDP · off by default",
    "c-wg-fig": "<svg class=\"tp tp-h\" viewBox=\"0 0 760 300\" role=\"img\" aria-label=\"WireGuard: the database host opens the tunnel to DumpCat through its firewall\"><defs><pattern id=\"br-wg-h\" width=\"24\" height=\"14\" patternUnits=\"userSpaceOnUse\"><rect class=\"tp-brick-bg\" width=\"24\" height=\"14\"/><path class=\"tp-brick-line\" d=\"M0 .7H24M0 7.7H24M6 .7V7.7M18 7.7V14\"/></pattern></defs><rect class=\"tp-panel\" x=\"12\" y=\"24\" width=\"228\" height=\"252\" rx=\"18\"/><use href=\"#dc-logo\" x=\"28\" y=\"40\" width=\"34\" height=\"34\"/><text class=\"tp-title\" x=\"74\" y=\"55\">DumpCat</text><text class=\"tp-sub\" x=\"74\" y=\"71\">Backup server</text><rect class=\"tp-chip\" x=\"28\" y=\"92\" width=\"196\" height=\"54\" rx=\"12\"/><rect class=\"tp-ico-bg\" x=\"38\" y=\"104\" width=\"30\" height=\"30\" rx=\"8\"/><svg class=\"tp-ico\" x=\"44\" y=\"110\" width=\"18\" height=\"18\" viewBox=\"0 0 24 24\" fill=\"none\" stroke=\"currentColor\" stroke-width=\"1.8\" stroke-linecap=\"round\" stroke-linejoin=\"round\" aria-hidden=\"true\"><rect x=\"3\" y=\"4\" width=\"18\" height=\"5\" rx=\"1\"/><path d=\"M5 9v10a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1V9M10 13h4\"/></svg><text class=\"tp-chip-t\" x=\"78\" y=\"115\">Backup &amp; restore</text><text class=\"tp-chip-s\" x=\"78\" y=\"132\">mysqldump · mydumper</text><path class=\"tp-conn\" d=\"M126 146V168\"/><text class=\"tp-mono\" x=\"134\" y=\"161\">127.0.0.1:port</text><rect class=\"tp-chip accent\" x=\"28\" y=\"168\" width=\"196\" height=\"54\" rx=\"12\"/><rect class=\"tp-ico-bg\" x=\"38\" y=\"180\" width=\"30\" height=\"30\" rx=\"8\"/><svg class=\"tp-ico\" x=\"44\" y=\"186\" width=\"18\" height=\"18\" viewBox=\"0 0 24 24\" fill=\"none\" stroke=\"currentColor\" stroke-width=\"1.8\" stroke-linecap=\"round\" stroke-linejoin=\"round\" aria-hidden=\"true\"><path d=\"M12 3l8 3v6c0 4.5-3.4 8.4-8 9-4.6-.6-8-4.5-8-9V6z\"/><path d=\"M9 12l2 2 4-4\"/></svg><text class=\"tp-chip-t\" x=\"78\" y=\"191\">WireGuard, built in</text><text class=\"tp-chip-s\" x=\"78\" y=\"208\">10.66.0.1 · UDP 51820</text><circle class=\"tp-ok-dot\" cx=\"33\" cy=\"252\" r=\"3.5\"/><text class=\"tp-foot\" x=\"43\" y=\"256\">UDP 51820 open</text><rect class=\"tp-panel\" x=\"520\" y=\"24\" width=\"228\" height=\"252\" rx=\"18\"/><rect class=\"tp-ico-bg big\" x=\"536\" y=\"40\" width=\"34\" height=\"34\" rx=\"9\"/><svg class=\"tp-ico\" x=\"544\" y=\"48\" width=\"18\" height=\"18\" viewBox=\"0 0 24 24\" fill=\"none\" stroke=\"currentColor\" stroke-width=\"1.8\" stroke-linecap=\"round\" stroke-linejoin=\"round\" aria-hidden=\"true\"><rect x=\"3\" y=\"4\" width=\"18\" height=\"7\" rx=\"2\"/><rect x=\"3\" y=\"13\" width=\"18\" height=\"7\" rx=\"2\"/><path d=\"M7 7.5h.01M7 16.5h.01\"/></svg><text class=\"tp-title\" x=\"582\" y=\"55\">Database host</text><text class=\"tp-sub\" x=\"582\" y=\"71\">No inbound port</text><rect class=\"tp-chip\" x=\"536\" y=\"92\" width=\"196\" height=\"54\" rx=\"12\"/><g class=\"tp-cyl\"><path d=\"M548 107v22c0 3.3 5.8 6 13 6s13-2.7 13-6v-22\"/><ellipse cx=\"561\" cy=\"107\" rx=\"13\" ry=\"5\"/><path d=\"M548 118c0 3.3 5.8 6 13 6s13-2.7 13-6\"/></g><text class=\"tp-chip-t\" x=\"586\" y=\"115\">MySQL / MariaDB</text><text class=\"tp-chip-s\" x=\"586\" y=\"132\">Listens on 10.66.0.2</text><path class=\"tp-conn\" d=\"M634 146V168\"/><text class=\"tp-mono\" x=\"642\" y=\"161\">10.66.0.2:3306</text><rect class=\"tp-chip accent\" x=\"536\" y=\"168\" width=\"196\" height=\"54\" rx=\"12\"/><rect class=\"tp-ico-bg\" x=\"546\" y=\"180\" width=\"30\" height=\"30\" rx=\"8\"/><svg class=\"tp-ico\" x=\"552\" y=\"186\" width=\"18\" height=\"18\" viewBox=\"0 0 24 24\" fill=\"none\" stroke=\"currentColor\" stroke-width=\"1.8\" stroke-linecap=\"round\" stroke-linejoin=\"round\" aria-hidden=\"true\"><path d=\"M12 3l8 3v6c0 4.5-3.4 8.4-8 9-4.6-.6-8-4.5-8-9V6z\"/><path d=\"M9 12l2 2 4-4\"/></svg><text class=\"tp-chip-t\" x=\"586\" y=\"191\">wg-quick@dumpcat</text><text class=\"tp-chip-s\" x=\"586\" y=\"208\">10.66.0.2 · keepalive 25 s</text><svg class=\"tp-ico\" x=\"536\" y=\"245\" width=\"14\" height=\"14\" viewBox=\"0 0 24 24\" fill=\"none\" stroke=\"currentColor\" stroke-width=\"1.8\" stroke-linecap=\"round\" stroke-linejoin=\"round\" aria-hidden=\"true\"><rect x=\"4\" y=\"10\" width=\"16\" height=\"11\" rx=\"2\"/><path d=\"M8 10V7a4 4 0 0 1 8 0v3\"/></svg><text class=\"tp-foot\" x=\"556\" y=\"256\">Nothing opened on this host</text><text class=\"tp-wall-t\" x=\"380\" y=\"30\" text-anchor=\"middle\">Firewall / NAT</text><rect class=\"tp-wall\" x=\"362\" y=\"38\" width=\"36\" height=\"226\" rx=\"6\" fill=\"url(#br-wg-h)\"/><rect class=\"tp-pipe\" x=\"222\" y=\"185\" width=\"316\" height=\"20\" rx=\"10\"/><rect class=\"tp-rim\" x=\"357\" y=\"180\" width=\"7\" height=\"30\" rx=\"3.5\"/><rect class=\"tp-rim\" x=\"396\" y=\"180\" width=\"7\" height=\"30\" rx=\"3.5\"/><g class=\"tp-dots\"><circle class=\"tp-dot out\" r=\"4\" cx=\"530\" cy=\"195\" opacity=\"0\"><set attributeName=\"opacity\" to=\"1\" begin=\"0.0s\"/><animateMotion dur=\"2.7s\" begin=\"0.0s\" repeatCount=\"indefinite\" path=\"M0 0L-300 0\"/></circle><circle class=\"tp-dot in\" r=\"3\" cx=\"230\" cy=\"195\" opacity=\"0\"><set attributeName=\"opacity\" to=\"1\" begin=\"0.45s\"/><animateMotion dur=\"2.7s\" begin=\"0.45s\" repeatCount=\"indefinite\" path=\"M0 0L300 0\"/></circle><circle class=\"tp-dot out\" r=\"4\" cx=\"530\" cy=\"195\" opacity=\"0\"><set attributeName=\"opacity\" to=\"1\" begin=\"0.9s\"/><animateMotion dur=\"2.7s\" begin=\"0.9s\" repeatCount=\"indefinite\" path=\"M0 0L-300 0\"/></circle><circle class=\"tp-dot in\" r=\"3\" cx=\"230\" cy=\"195\" opacity=\"0\"><set attributeName=\"opacity\" to=\"1\" begin=\"1.35s\"/><animateMotion dur=\"2.7s\" begin=\"1.35s\" repeatCount=\"indefinite\" path=\"M0 0L300 0\"/></circle><circle class=\"tp-dot out\" r=\"4\" cx=\"530\" cy=\"195\" opacity=\"0\"><set attributeName=\"opacity\" to=\"1\" begin=\"1.8s\"/><animateMotion dur=\"2.7s\" begin=\"1.8s\" repeatCount=\"indefinite\" path=\"M0 0L-300 0\"/></circle><circle class=\"tp-dot in\" r=\"3\" cx=\"230\" cy=\"195\" opacity=\"0\"><set attributeName=\"opacity\" to=\"1\" begin=\"2.25s\"/><animateMotion dur=\"2.7s\" begin=\"2.25s\" repeatCount=\"indefinite\" path=\"M0 0L300 0\"/></circle></g><rect class=\"tp-pill\" x=\"244\" y=\"148\" width=\"272\" height=\"24\" rx=\"12\"/><text class=\"tp-pill-t\" x=\"380\" y=\"164\" text-anchor=\"middle\">← WireGuard UDP · opened by the database host</text><path class=\"tp-block\" d=\"M240 244H346\"/><circle class=\"tp-x-bg\" cx=\"356\" cy=\"244\" r=\"10\"/><path class=\"tp-x\" d=\"M352 240l8 8M360 240l-8 8\"/><text class=\"tp-block-t\" x=\"293\" y=\"236\" text-anchor=\"middle\">3306 inbound</text><text class=\"tp-block-s\" x=\"293\" y=\"262\" text-anchor=\"middle\">blocked, not needed</text></svg><svg class=\"tp tp-v\" viewBox=\"0 0 340 640\" role=\"img\" aria-label=\"WireGuard: the database host opens the tunnel to DumpCat through its firewall\"><defs><pattern id=\"br-wg-v\" width=\"24\" height=\"14\" patternUnits=\"userSpaceOnUse\"><rect class=\"tp-brick-bg\" width=\"24\" height=\"14\"/><path class=\"tp-brick-line\" d=\"M0 .7H24M0 7.7H24M6 .7V7.7M18 7.7V14\"/></pattern></defs><rect class=\"tp-panel\" x=\"10\" y=\"10\" width=\"320\" height=\"240\" rx=\"18\"/><use href=\"#dc-logo\" x=\"26\" y=\"26\" width=\"34\" height=\"34\"/><text class=\"tp-title\" x=\"72\" y=\"41\">DumpCat</text><text class=\"tp-sub\" x=\"72\" y=\"57\">Backup server</text><rect class=\"tp-chip\" x=\"26\" y=\"78\" width=\"288\" height=\"54\" rx=\"12\"/><rect class=\"tp-ico-bg\" x=\"36\" y=\"90\" width=\"30\" height=\"30\" rx=\"8\"/><svg class=\"tp-ico\" x=\"42\" y=\"96\" width=\"18\" height=\"18\" viewBox=\"0 0 24 24\" fill=\"none\" stroke=\"currentColor\" stroke-width=\"1.8\" stroke-linecap=\"round\" stroke-linejoin=\"round\" aria-hidden=\"true\"><rect x=\"3\" y=\"4\" width=\"18\" height=\"5\" rx=\"1\"/><path d=\"M5 9v10a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1V9M10 13h4\"/></svg><text class=\"tp-chip-t\" x=\"76\" y=\"101\">Backup &amp; restore</text><text class=\"tp-chip-s\" x=\"76\" y=\"118\">mysqldump · mydumper</text><path class=\"tp-conn\" d=\"M120 132V154\"/><text class=\"tp-mono\" x=\"128\" y=\"147\">127.0.0.1:port</text><rect class=\"tp-chip accent\" x=\"26\" y=\"154\" width=\"288\" height=\"54\" rx=\"12\"/><rect class=\"tp-ico-bg\" x=\"36\" y=\"166\" width=\"30\" height=\"30\" rx=\"8\"/><svg class=\"tp-ico\" x=\"42\" y=\"172\" width=\"18\" height=\"18\" viewBox=\"0 0 24 24\" fill=\"none\" stroke=\"currentColor\" stroke-width=\"1.8\" stroke-linecap=\"round\" stroke-linejoin=\"round\" aria-hidden=\"true\"><path d=\"M12 3l8 3v6c0 4.5-3.4 8.4-8 9-4.6-.6-8-4.5-8-9V6z\"/><path d=\"M9 12l2 2 4-4\"/></svg><text class=\"tp-chip-t\" x=\"76\" y=\"177\">WireGuard, built in</text><text class=\"tp-chip-s\" x=\"76\" y=\"194\">10.66.0.1 · UDP 51820</text><circle class=\"tp-ok-dot\" cx=\"31\" cy=\"232\" r=\"3.5\"/><text class=\"tp-foot\" x=\"41\" y=\"236\">UDP 51820 open</text><rect class=\"tp-wall\" x=\"20\" y=\"298\" width=\"300\" height=\"32\" rx=\"6\" fill=\"url(#br-wg-v)\"/><text class=\"tp-wall-t\" x=\"318\" y=\"350\" text-anchor=\"end\">Firewall / NAT</text><rect class=\"tp-pipe\" x=\"160\" y=\"204\" width=\"20\" height=\"248\" rx=\"10\"/><rect class=\"tp-rim\" x=\"155\" y=\"293\" width=\"30\" height=\"7\" rx=\"3.5\"/><rect class=\"tp-rim\" x=\"155\" y=\"328\" width=\"30\" height=\"7\" rx=\"3.5\"/><g class=\"tp-dots\"><circle class=\"tp-dot out\" r=\"4\" cx=\"170\" cy=\"446\" opacity=\"0\"><set attributeName=\"opacity\" to=\"1\" begin=\"0.0s\"/><animateMotion dur=\"2.7s\" begin=\"0.0s\" repeatCount=\"indefinite\" path=\"M0 0L0 -234\"/></circle><circle class=\"tp-dot in\" r=\"3\" cx=\"170\" cy=\"212\" opacity=\"0\"><set attributeName=\"opacity\" to=\"1\" begin=\"0.45s\"/><animateMotion dur=\"2.7s\" begin=\"0.45s\" repeatCount=\"indefinite\" path=\"M0 0L0 234\"/></circle><circle class=\"tp-dot out\" r=\"4\" cx=\"170\" cy=\"446\" opacity=\"0\"><set attributeName=\"opacity\" to=\"1\" begin=\"0.9s\"/><animateMotion dur=\"2.7s\" begin=\"0.9s\" repeatCount=\"indefinite\" path=\"M0 0L0 -234\"/></circle><circle class=\"tp-dot in\" r=\"3\" cx=\"170\" cy=\"212\" opacity=\"0\"><set attributeName=\"opacity\" to=\"1\" begin=\"1.35s\"/><animateMotion dur=\"2.7s\" begin=\"1.35s\" repeatCount=\"indefinite\" path=\"M0 0L0 234\"/></circle><circle class=\"tp-dot out\" r=\"4\" cx=\"170\" cy=\"446\" opacity=\"0\"><set attributeName=\"opacity\" to=\"1\" begin=\"1.8s\"/><animateMotion dur=\"2.7s\" begin=\"1.8s\" repeatCount=\"indefinite\" path=\"M0 0L0 -234\"/></circle><circle class=\"tp-dot in\" r=\"3\" cx=\"170\" cy=\"212\" opacity=\"0\"><set attributeName=\"opacity\" to=\"1\" begin=\"2.25s\"/><animateMotion dur=\"2.7s\" begin=\"2.25s\" repeatCount=\"indefinite\" path=\"M0 0L0 234\"/></circle></g><rect class=\"tp-pill\" x=\"62\" y=\"256\" width=\"216\" height=\"34\" rx=\"12\"/><text class=\"tp-pill-t\" x=\"170\" y=\"270\" text-anchor=\"middle\">↑ WireGuard UDP</text><text class=\"tp-pill-s\" x=\"170\" y=\"284\" text-anchor=\"middle\">opened by the database host</text><path class=\"tp-block\" d=\"M40 250V288\"/><circle class=\"tp-x-bg\" cx=\"40\" cy=\"298\" r=\"10\"/><path class=\"tp-x\" d=\"M36 294l8 8M44 294l-8 8\"/><text class=\"tp-block-t\" x=\"22\" y=\"352\">3306 inbound</text><text class=\"tp-block-s\" x=\"22\" y=\"367\">blocked, not needed</text><rect class=\"tp-panel\" x=\"10\" y=\"380\" width=\"320\" height=\"250\" rx=\"18\"/><rect class=\"tp-ico-bg big\" x=\"26\" y=\"396\" width=\"34\" height=\"34\" rx=\"9\"/><svg class=\"tp-ico\" x=\"34\" y=\"404\" width=\"18\" height=\"18\" viewBox=\"0 0 24 24\" fill=\"none\" stroke=\"currentColor\" stroke-width=\"1.8\" stroke-linecap=\"round\" stroke-linejoin=\"round\" aria-hidden=\"true\"><rect x=\"3\" y=\"4\" width=\"18\" height=\"7\" rx=\"2\"/><rect x=\"3\" y=\"13\" width=\"18\" height=\"7\" rx=\"2\"/><path d=\"M7 7.5h.01M7 16.5h.01\"/></svg><text class=\"tp-title\" x=\"72\" y=\"411\">Database host</text><text class=\"tp-sub\" x=\"72\" y=\"427\">No inbound port</text><rect class=\"tp-chip accent\" x=\"26\" y=\"448\" width=\"288\" height=\"54\" rx=\"12\"/><rect class=\"tp-ico-bg\" x=\"36\" y=\"460\" width=\"30\" height=\"30\" rx=\"8\"/><svg class=\"tp-ico\" x=\"42\" y=\"466\" width=\"18\" height=\"18\" viewBox=\"0 0 24 24\" fill=\"none\" stroke=\"currentColor\" stroke-width=\"1.8\" stroke-linecap=\"round\" stroke-linejoin=\"round\" aria-hidden=\"true\"><path d=\"M12 3l8 3v6c0 4.5-3.4 8.4-8 9-4.6-.6-8-4.5-8-9V6z\"/><path d=\"M9 12l2 2 4-4\"/></svg><text class=\"tp-chip-t\" x=\"76\" y=\"471\">wg-quick@dumpcat</text><text class=\"tp-chip-s\" x=\"76\" y=\"488\">10.66.0.2 · keepalive 25 s</text><path class=\"tp-conn\" d=\"M120 502V524\"/><text class=\"tp-mono\" x=\"128\" y=\"517\">10.66.0.2:3306</text><rect class=\"tp-chip\" x=\"26\" y=\"524\" width=\"288\" height=\"54\" rx=\"12\"/><g class=\"tp-cyl\"><path d=\"M38 539v22c0 3.3 5.8 6 13 6s13-2.7 13-6v-22\"/><ellipse cx=\"51\" cy=\"539\" rx=\"13\" ry=\"5\"/><path d=\"M38 550c0 3.3 5.8 6 13 6s13-2.7 13-6\"/></g><text class=\"tp-chip-t\" x=\"76\" y=\"547\">MySQL / MariaDB</text><text class=\"tp-chip-s\" x=\"76\" y=\"564\">Listens on 10.66.0.2</text><svg class=\"tp-ico\" x=\"26\" y=\"599\" width=\"14\" height=\"14\" viewBox=\"0 0 24 24\" fill=\"none\" stroke=\"currentColor\" stroke-width=\"1.8\" stroke-linecap=\"round\" stroke-linejoin=\"round\" aria-hidden=\"true\"><rect x=\"4\" y=\"10\" width=\"16\" height=\"11\" rx=\"2\"/><path d=\"M8 10V7a4 4 0 0 1 8 0v3\"/></svg><text class=\"tp-foot\" x=\"46\" y=\"610\">Nothing opened on this host</text></svg><figcaption class=\"tp-legend\"><span><i class=\"out\"></i>The database host opens the tunnel</span><span><i class=\"in\"></i>DumpCat&#x27;s database traffic flows back inside it</span></figcaption>",
    "c-wg-facts": "<div><dt>The database host needs</dt><dd>Standard WireGuard, outbound UDP 51820 to DumpCat</dd></div><div><dt>MySQL</dt><dd>Listens on the VPN address, the user allows 10.66.0.1</dd></div><div><dt>Install</dt><dd>Turn it on on the WireGuard page, copy <code>dumpcat.conf</code>, <code>systemctl enable --now wg-quick@dumpcat</code></dd></div><div><dt>Good for</dt><dd>Teams that already use WireGuard, or a gateway to a whole database network</dd></div>",
    "c-note": "The database host opens the connection; the data flows both ways inside it. Physical backups (XtraBackup / MariaBackup) work through an agent started with <code>--allow-physical</code>; through WireGuard they still need DumpCat installed on the database host.",
    "o-label": "Operations",
    "o-title": "The parts you would otherwise script yourself",
    "o-intro": "All of it ships with DumpCat and is switched on and tuned from the web UI.",
    "o-list": "<div><dt>Encrypted before upload</dt><dd>age (ChaCha20-Poly1305 + scrypt) runs on your server, so the cloud provider cannot read your data.</dd></div>" +
      "<div><dt>Integrity checks</dt><dd><span class=\"nw\">SHA-256</span> for every file. Verify re-reads, decrypts, decompresses and looks for the <code>Dump completed</code> line.</dd></div>" +
      "<div><dt>GFS retention</dt><dd>Keep N backups, N days, the last one of each day, week and month. Incremental chains go only with their full.</dd></div>" +
      "<div><dt>Careful restores</dt><dd>Every file is test-decrypted before any database is dropped. Restore under new names, or onto another server and version (MySQL 8 ↔ MariaDB).</dd></div>" +
      "<div><dt>Accounts and roles</dt><dd>admin, operator, viewer. TOTP two-factor, API tokens, an audit log and lockout after repeated failed logins.</dd></div>" +
      "<div><dt>Notifications</dt><dd>Email, Telegram, Slack, Discord and HMAC-signed webhooks, on failure or on success.</dd></div>" +
      "<div><dt>Databases behind a firewall</dt><dd>An agent on the database host connects out to DumpCat over HTTPS, or the host joins the built-in WireGuard. No port has to be opened to the database.</dd></div>" +
      "<div><dt>Self-update</dt><dd><code>dumpcat update</code> checks <span class=\"nw\">SHA-256</span>, backs up the configuration, waits for running backups and rolls back on failure.</dd></div>",
    "oss-label": "Open source",
    "oss-title": "Free, open source, and your data stays yours",
    "oss-intro": "DumpCat is released under the Apache-2.0 license. No account, no license key, no paid edition holding features back: everything on this page is in the build you download.",
    "oss-list": "<div><dt>Apache-2.0 license</dt><dd>Use it at home or at work, modify it and redistribute it. The full text is in <a href=\"https://github.com/theweakestharrison/DumpCat/blob/main/LICENSE\">LICENSE</a>.</dd></div>" +
      "<div><dt>No telemetry</dt><dd>DumpCat sends no usage data anywhere. The only call it makes on its own is reading <code>latest.json</code> once a day to announce new versions, and you can switch that off in Settings.</dd></div>" +
      "<div><dt>No middleman</dt><dd>Backups, database passwords and encryption keys travel only from your server to the storage you choose.</dd></div>" +
      "<div><dt>Signed releases</dt><dd>Every update is signed with ed25519 and checked against <span class=\"nw\">SHA-256</span> before it is installed. Building it yourself only takes Go.</dd></div>",
    "oss-build-title": "Build from source",
    "oss-contrib": "Bug reports, pull requests and translations are welcome, see <a href=\"https://github.com/theweakestharrison/DumpCat/blob/main/CONTRIBUTING.md\">CONTRIBUTING.md</a>. Please report security problems privately, as described in <a href=\"https://github.com/theweakestharrison/DumpCat/blob/main/SECURITY.md\">SECURITY.md</a>.",
    "oss-github": "View on GitHub",
    "oss-issues": "Report an issue",
    "cmp-label": "Compare",
    "cmp-title": "DumpCat next to the other tools",
    "cmp-intro": "Pick a tool to see every feature side by side. Facts about other products are sourced and dated on the full comparison pages.",
    "d-label": "Download",
    "d-title": "Latest release",
    "d-linux": "amd64 and arm64, installer, systemd service and manual",
    "d-windows": "64-bit, installer for the Windows service and manual",
    "d-next": "Downloaded it? Open <b>START-HERE.txt</b> in the package, or follow the <a href=\"/install/#downloaded\">step-by-step install guide</a>.",
    "d-bins": "Single binaries",
    "d-old": "Older versions stay available at <code>downloads.dumpcat.cc/&lt;version&gt;/</code>.",
    "i-label": "Install",
    "i-title": "Running in a minute",
    "i-intro": "One command on Linux, download and double-click on Windows. The installer sets up a service that starts with the machine and the backup tools that match your database version; the web UI is built into a single file.",
    "i-linux-lead": "On the Linux server, paste this command and press Enter:",
    "i-linux-note": "It downloads the latest release, checks its checksum and installs it to <code>/opt/dumpcat</code> as a systemd service, with <code>rclone</code>, the MySQL/MariaDB client, the matching mariabackup or XtraBackup, and mydumper when the repositories have it. Options go after <code>sh -s --</code>: <code>--port 9090</code>, <code>--listen 127.0.0.1</code>, <code>--tls-auto</code>.",
    "i-win-1": "<b>Download the .zip</b>",
    "i-win-dl": "Download for Windows",
    "i-win-2": "<b>Unzip it</b>: right-click the file, choose <i>Extract All</i>.",
    "i-win-3": "<b>Double-click <i>Install DumpCat</i></b> and answer <i>Yes</i>. Your browser opens DumpCat; the install window shows the setup token.",
    "i-win-note": "Windows 10 / 11 or Windows Server 2016+ 64-bit, runs as a Windows service. Prefer PowerShell? Run <code>irm https://dumpcat.cc/install.ps1 | iex</code> in PowerShell (Administrator). mydumper and XtraBackup for MySQL are not available; run DumpCat on Linux for databases of hundreds of GB.",
    "i-guide": "New to the command line? <a href=\"/install/\">Follow the step-by-step guide</a>, with fixes for common problems.",
    "i-steps-title": "After installing",
    "i-steps": "<li><div><b>Open <code>http://&lt;server-ip&gt;:6369</code></b><span>Create the admin account with the one-time setup token the installer prints (show it again: <code>dumpcat setup-token</code>).</span></div></li>" +
      "<li><div><b>Add a database server</b><span>Test connection lists missing privileges and the methods you can use.</span></div></li>" +
      "<li><div><b>Add a storage</b><span>Test writes, reads and deletes a file.</span></div></li>" +
      "<li><div><b>Create a backup job</b><span>Pick a schedule, encryption, storage and retention, then Save &amp; run now.</span></div></li>",
    "meow-title": "Port 6369 spells <em>MEOW</em>",
    "meow": "Type 6-3-6-9 on a phone keypad and you get MEOW. It is the default port of the web UI.",
    "foot": "Open source under the Apache-2.0 license, with the source on <a href=\"https://github.com/theweakestharrison/DumpCat\">GitHub</a>. <a class=\"compare-link\" href=\"/compare/\">How it compares with other tools</a>. The full manual, in English and Vietnamese, ships inside every package.",
    "foot-mail": "Support: <a href=\"mailto:fat@dumpcat.cc\">fat@dumpcat.cc</a>"
  };

  var STR = {
    vi: { aria: "Thời điểm khôi phục", consistent: "mysqldump · nhất quán lúc", files: function (n) { return n + " file, kiểm tra từng file trước khi đụng vào dữ liệu"; }, ok: "Trước sự cố: bảng orders còn nguyên", bad: "Sau sự cố: bảng orders đã bị xóa", copy: "Sao chép", copied: "Đã chép", selected: "Đã chọn, nhấn Ctrl+C",
      relLoading: "Đang tải thông tin bản phát hành…",
      relIntro: "Phiên bản {v}, phát hành ngày {d}. Mỗi gói có file chạy, script cài đặt và hướng dẫn đầy đủ; SHA-256 của mọi file nằm trong {sums}.",
      relFailed: "Chưa tải được thông tin bản phát hành. Phiên bản mới nhất và các file được liệt kê trong {feed}.", dateLocale: "vi-VN" },
    en: { aria: "Recovery point", consistent: "mysqldump · consistent at", files: function (n) { return n + (n === 1 ? " file" : " files") + ", each checked before any data is touched"; }, ok: "Before the incident: orders is intact", bad: "After the incident: orders is already gone", copy: "Copy", copied: "Copied", selected: "Selected, press Ctrl+C",
      relLoading: "Loading the latest release…",
      relIntro: "Version {v}, released {d}. Each package holds the binaries, the installer and the full manual; SHA-256 checksums of every file are in {sums}.",
      relFailed: "The release details could not be loaded. The latest version and its files are listed in {feed}.", dateLocale: "en" }
  };

  // ---- PITR timeline model (seconds since 00:00 on 18/10/2026) ----
  var AXIS_END = 18 * 3600, NOW = 16 * 3600 + 20 * 60;
  var BACKUP_AT = 2 * 3600, CONSISTENT = BACKUP_AT + 3, INCIDENT = 14 * 3600 + 37 * 60 + 12;
  var FILE_SECONDS = 900, FILE_TODAY = 123, FILE_YESTERDAY = FILE_TODAY - 86400 / FILE_SECONDS;
  var lang = "vi";
  var t = INCIDENT - 1;

  var range = document.getElementById("tl-range");
  var replay = document.getElementById("tl-replay");
  var roTo = document.getElementById("ro-to");
  var roBase = document.getElementById("ro-base");
  var roReplay = document.getElementById("ro-replay");
  var roStatus = document.getElementById("ro-status");

  function pad(n, w) { n = String(n); while (n.length < w) n = "0" + n; return n; }
  function hms(s) { return pad(Math.floor(s / 3600), 2) + ":" + pad(Math.floor(s / 60) % 60, 2) + ":" + pad(s % 60, 2); }
  function pct(s) { return (s / AXIS_END * 100).toFixed(4) + "%"; }
  function binlog(i) { return "mysql-bin." + pad(i, 6); }

  function render() {
    var s = STR[lang];
    var today = t >= CONSISTENT;
    var baseAt = today ? CONSISTENT : CONSISTENT - 86400;
    var first = today ? FILE_TODAY : FILE_YESTERDAY;
    var last = first + Math.floor((t - baseAt) / FILE_SECONDS);
    var start = today ? BACKUP_AT : 0;

    replay.style.left = pct(start);
    replay.style.width = pct(Math.max(0, t - start));
    roTo.textContent = "18/10/2026 " + hms(t);
    roBase.innerHTML = (today ? "20261018-020000-full" : "20261017-020000-full") + "<small>" + s.consistent + " " + (today ? "" : "17/10 ") + hms(CONSISTENT) + "</small>";
    roReplay.innerHTML = binlog(first) + " → " + binlog(last) + "<small>" + s.files(last - first + 1) + "</small>";
    var ok = t < INCIDENT;
    roStatus.className = "status " + (ok ? "ok" : "bad");
    roStatus.textContent = ok ? s.ok : s.bad;
    range.setAttribute("aria-label", s.aria);
    range.setAttribute("aria-valuetext", hms(t));
  }

  function setT(v) {
    t = Math.max(0, Math.min(NOW, Math.round(v)));
    range.value = String(t);
    render();
  }

  range.addEventListener("input", function () { setT(Number(range.value)); });
  range.addEventListener("keydown", function (e) {
    var step = { ArrowLeft: -60, ArrowDown: -60, ArrowRight: 60, ArrowUp: 60, PageDown: -900, PageUp: 900 }[e.key];
    if (step) { e.preventDefault(); setT(t + (e.shiftKey ? step * 15 : step)); }
  });
  document.getElementById("tl-snap").addEventListener("click", function () { setT(INCIDENT - 1); range.focus(); });

  // ---- language ----
  var viCache = new Map();
  function applyLang(l) {
    lang = l === "en" ? "en" : "vi";
    document.querySelectorAll("[data-i18n]").forEach(function (el) {
      if (!viCache.has(el)) viCache.set(el, el.innerHTML);
      var en = EN[el.getAttribute("data-i18n")];
      el.innerHTML = lang === "en" && en != null ? en : viCache.get(el);
    });
    document.documentElement.lang = lang;
    document.querySelectorAll(".lang button").forEach(function (b) { b.setAttribute("aria-pressed", String(b.getAttribute("data-lang") === lang)); });
    document.querySelectorAll(".copy").forEach(function (b) { b.textContent = STR[lang].copy; });
    document.querySelectorAll(".compare-link").forEach(function (a) { a.setAttribute("href", lang === "vi" ? "/vi/compare/" : "/compare/"); });
    document.querySelectorAll("[data-vi-href]").forEach(function (a) {
      if (!a.hasAttribute("data-en-href")) a.setAttribute("data-en-href", a.getAttribute("href"));
      a.setAttribute("href", a.getAttribute(lang === "vi" ? "data-vi-href" : "data-en-href"));
    });
    render();
    renderRelease();
  }
  document.querySelectorAll(".lang button").forEach(function (b) {
    b.addEventListener("click", function () {
      applyLang(b.getAttribute("data-lang"));
      try { localStorage.setItem("dumpcat-landing-lang", lang); } catch (e) {}
    });
  });

  // ---- latest release: read from the update feed, so a new upload shows here without redeploying the site ----
  var relMeta = document.querySelector('meta[name="dumpcat-release-base"]');
  var relBase = new URL(relMeta ? relMeta.content : "https://downloads.dumpcat.cc/", location.href);
  if (!/\/$/.test(relBase.pathname)) relBase.pathname += "/";
  var relFeed = new URL("latest.json", relBase);
  var release = null, releaseFailed = false;
  var PLATFORMS = { "linux/amd64": "Linux x86-64", "linux/arm64": "Linux ARM64", "windows/amd64": "Windows x64" };

  // Links only to the release server itself: a tampered feed must not turn this page into a link to elsewhere.
  function relURL(ref) {
    var u = new URL(ref, relFeed);
    if (u.origin !== relBase.origin || u.pathname.indexOf(relBase.pathname) !== 0) throw new Error("file outside the release server: " + ref);
    return u.href;
  }
  function node(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  }
  function link(href, text) { var a = node("a", "", text); a.href = href; return a; }
  // "{name}" placeholders in a sentence become the given nodes or strings.
  function fill(el, tpl, parts) {
    el.textContent = "";
    tpl.split(/\{(\w+)\}/).forEach(function (piece, i) {
      if (i % 2 === 0) { if (piece) el.appendChild(document.createTextNode(piece)); return; }
      var p = parts[piece];
      el.appendChild(typeof p === "string" ? document.createTextNode(p) : p);
    });
  }
  function renderRelease() {
    var s = STR[lang];
    var intro = document.getElementById("rel-intro");
    if (releaseFailed) { fill(intro, s.relFailed, { feed: link(relFeed.href, "latest.json") }); return; }
    if (!release) { intro.textContent = s.relLoading; return; }
    var v = release.version;
    document.querySelectorAll("[data-version]").forEach(function (e) { e.textContent = v; });
    document.querySelectorAll("[data-base]").forEach(function (e) { e.textContent = relBase.href; });
    var when = new Date(release.released_at);
    var date = isNaN(when) ? "" : new Intl.DateTimeFormat(s.dateLocale, { dateStyle: "long" }).format(when);
    fill(intro, date ? s.relIntro : s.relIntro.replace(/, [^{]*\{d\}/, ""), {
      v: node("b", "", v), d: date, sums: link(relURL(v + "/SHA256SUMS"), "SHA256SUMS")
    });
  }

  function showRelease(rel) {
    var v = String(rel && rel.version || "");
    if (!/^[0-9A-Za-z][0-9A-Za-z.+-]{0,63}$/.test(v)) throw new Error("bad version in latest.json");

    var linux = "dumpcat-" + v + "-linux.tar.gz", windows = "dumpcat-" + v + "-windows-amd64.zip";
    document.getElementById("pkg-linux").href = relURL(v + "/" + linux);
    document.getElementById("pkg-linux-file").textContent = linux;
    document.getElementById("pkg-windows").href = relURL(v + "/" + windows);
    document.getElementById("pkg-windows-file").textContent = windows;
    document.querySelectorAll("a[data-pkg]").forEach(function (a) {
      a.href = relURL(v + "/" + (a.getAttribute("data-pkg") === "windows" ? windows : linux));
    });

    // The What's new card is built from CHANGELOG.md with the site; a release uploaded without
    // deploying the site has no page yet, so the card of an older version is not shown.
    var wn = document.querySelector("[data-wn-version]");
    if (wn) wn.hidden = wn.getAttribute("data-wn-version") !== v;

    var bins = document.getElementById("rel-bins");
    bins.textContent = "";
    Object.keys(rel.files || {}).sort().forEach(function (key) {
      var f = rel.files[key];
      if (!f || typeof f.url !== "string") return;
      var href = relURL(f.url);
      var li = node("li");
      li.appendChild(node("b", "", PLATFORMS[key] || key));
      li.appendChild(link(href, href.slice(href.lastIndexOf("/") + 1)));
      li.appendChild(node("span", "size", typeof f.size === "number" && f.size > 0 ? (f.size / 1e6).toFixed(1) + " MB" : ""));
      if (/^[0-9a-f]{64}$/i.test(f.sha256 || "")) li.appendChild(node("span", "sha", "SHA-256 " + f.sha256.toLowerCase()));
      bins.appendChild(li);
    });

    release = { version: v, released_at: rel.released_at };
    document.getElementById("release").hidden = false;
    renderRelease();
  }

  fetch(relFeed.href, { credentials: "omit" })
    .then(function (r) { if (!r.ok) throw new Error("HTTP " + r.status + " for " + relFeed.href); return r.json(); })
    .then(showRelease)
    .catch(function (err) {
      releaseFailed = true;
      renderRelease();
      if (window.console) console.warn("dumpcat.cc: " + err);
    });

  // ---- install tabs ----
  var tabs = Array.prototype.slice.call(document.querySelectorAll('[role="tab"]'));
  function selectTab(tab) {
    tabs.forEach(function (x) {
      var on = x === tab;
      x.setAttribute("aria-selected", String(on));
      x.tabIndex = on ? 0 : -1;
      document.getElementById(x.getAttribute("aria-controls")).hidden = !on;
    });
  }
  tabs.forEach(function (tab, i) {
    tab.addEventListener("click", function () { selectTab(tab); });
    tab.addEventListener("keydown", function (e) {
      if (e.key !== "ArrowRight" && e.key !== "ArrowLeft") return;
      var next = tabs[(i + (e.key === "ArrowRight" ? 1 : tabs.length - 1)) % tabs.length];
      selectTab(next); next.focus();
    });
  });

  // ---- comparison: one tool at a time next to DumpCat ----
  document.querySelectorAll(".lc").forEach(function (box) {
    box.querySelectorAll(".lc-chip").forEach(function (chip) {
      chip.addEventListener("click", function () {
        var tool = chip.getAttribute("data-tool");
        box.querySelectorAll(".lc-chip").forEach(function (c) { c.setAttribute("aria-pressed", String(c === chip)); });
        box.querySelectorAll("[data-tool]:not(.lc-chip)").forEach(function (e) { e.hidden = e.getAttribute("data-tool") !== tool; });
      });
    });
  });

  // ---- copy buttons ----
  document.querySelectorAll(".copy").forEach(function (btn) {
    btn.addEventListener("click", function () {
      var code = document.getElementById(btn.getAttribute("data-target"));
      var text = code.textContent;
      function flash(msg) { btn.textContent = msg; setTimeout(function () { btn.textContent = STR[lang].copy; }, 1600); }
      function selectIt() {
        var r = document.createRange(); r.selectNodeContents(code);
        var sel = window.getSelection(); sel.removeAllRanges(); sel.addRange(r);
        flash(STR[lang].selected);
      }
      try {
        navigator.clipboard.writeText(text).then(function () { flash(STR[lang].copied); }, selectIt);
      } catch (e) { selectIt(); }
    });
  });

  var saved = null;
  try { saved = localStorage.getItem("dumpcat-landing-lang"); } catch (e) {}
  applyLang(saved === "vi" ? "vi" : "en");
})();
