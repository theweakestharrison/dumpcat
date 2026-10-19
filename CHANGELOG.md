# Changelog

The section of the version being built becomes the release notes shown in the
web UI and by `dumpcat update` (see `build.sh`). Newest version first.

## 1.1.0

- Restore onto another instance: the restore form connects to the chosen server first and shows its version, flavor
  and the databases already on it, warns about every database that would be overwritten (not only on the source
  server), and lets you add a new target server without leaving the page.
- Compatibility options for logical restores to another instance or version, suggested from the target: remove
  `DEFINER` from views, routines, triggers and events; convert MySQL 8 `utf8mb4_0900_*`, MariaDB `uca1400` and
  `utf8mb3_*` collations (MySQL 8 → MariaDB / 5.7 and back); skip `SET @@GLOBAL.GTID_PURGED`. Only schema statements
  are rewritten, row data is never changed; mydumper schema files are rewritten after extraction.
- Physical restores: choosing the instance now fills its datadir, service and owner, and the form warns when the
  target runs another version series or flavor.
- New API endpoint `POST /api/runs/{id}/restore-check`.
- Fix: the GTID set of MySQL 8 mysqldump backups (`GTID_PURGED=/*!80000 '+'*/ '…'`) is now recorded, so
  point-in-time recovery to a GTID can tell when the target is already inside the backup.

## 1.0.0

- First release of DumpCat: MySQL/MariaDB backup & restore with a web UI.
- Lock-free logical backups (mysqldump/mariadb-dump, mydumper) and hot physical backups (XtraBackup/mariabackup).
- Storages: local/NFS, S3, Google Drive, OneDrive, FTP/FTPS, SFTP, WebDAV and any rclone backend.
- Binlog archiving and point-in-time recovery.
- Linux and Windows (service) installers; built-in updates with automatic rollback.
