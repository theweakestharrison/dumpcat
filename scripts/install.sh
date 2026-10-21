#!/usr/bin/env bash
# DumpCat installer — MySQL, MariaDB & PostgreSQL backup & restore manager
#
#   sudo ./install.sh                     # install binary + systemd + backup tools
#   sudo ./install.sh --port 9090         # custom port
#   sudo ./install.sh --postgres 16       # also the PostgreSQL 16 client tools (majors of your servers: 15,16)
#   sudo ./install.sh --no-deps           # do not install backup tools
#   sudo ./install.sh --tls-auto          # serve HTTPS with a self-signed certificate
#   sudo ./install.sh --uninstall [--purge]
#
# Agent only, on a database host that accepts no inbound connections (it connects out to DumpCat):
#   sudo ./install.sh --agent --server https://backup.example.com --key dca_...
#        [--allow 127.0.0.1:3306,unix:/run/mysqld/mysqld.sock] [--insecure]   # PostgreSQL: 127.0.0.1:5432
#        [--allow-physical]   # also run physical backups (mariabackup/xtrabackup) here
#        [--allow-mssql-dir /var/opt/mssql/backup]   # SQL Server: move its backup files for DumpCat (--allow 127.0.0.1:1433)
#   sudo ./install.sh --agent --uninstall [--purge]
#
# Supported: Ubuntu 20.04+, Debian 11+, RHEL/Rocky/Alma/Oracle 8+, CentOS Stream, Fedora.
set -euo pipefail

PORT=6369
LISTEN_ADDR="0.0.0.0"
INSTALL_DEPS=1
TLS_AUTO=0
UNINSTALL=0
PURGE=0
BINARY=""
DATA_DIR=/var/lib/dumpcat
CONF_DIR=/etc/dumpcat
# the binary lives in /opt/dumpcat so that the service (ProtectSystem=full keeps
# /usr read-only) can replace it when installing an update
BIN_DIR=/opt/dumpcat
BIN_PATH=$BIN_DIR/dumpcat
BIN_LINK=/usr/local/bin/dumpcat
UNIT=/etc/systemd/system/dumpcat.service
DOC_DIR=/usr/local/share/doc/dumpcat
AGENT=0
AGENT_SERVER=""
AGENT_KEY=""
AGENT_ALLOW=""
AGENT_INSECURE=0
AGENT_PHYSICAL=0
AGENT_MSSQL_DIR=""
PG_MAJORS=""
AGENT_UNIT=/etc/systemd/system/dumpcat-agent.service
AGENT_DROPIN=/etc/systemd/system/dumpcat-agent.service.d/physical.conf
AGENT_MSSQL_DROPIN=/etc/systemd/system/dumpcat-agent.service.d/mssql.conf
AGENT_ENV=$CONF_DIR/agent.env
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

c_green=$'\e[32m'; c_yellow=$'\e[33m'; c_red=$'\e[31m'; c_bold=$'\e[1m'; c_off=$'\e[0m'
info()  { echo "${c_green}==>${c_off} $*"; }
warn()  { echo "${c_yellow}!!${c_off} $*" >&2; }
die()   { echo "${c_red}xx${c_off} $*" >&2; exit 1; }

usage() { sed -n '2,18p' "$0" | sed 's/^# \{0,1\}//'; exit 0; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --port) PORT="$2"; shift 2 ;;
    --listen) LISTEN_ADDR="$2"; shift 2 ;;
    --no-deps) INSTALL_DEPS=0; shift ;;
    --postgres) PG_MAJORS="$2"; shift 2 ;;
    --tls-auto) TLS_AUTO=1; shift ;;
    --binary) BINARY="$2"; shift 2 ;;
    --data-dir) DATA_DIR="$2"; shift 2 ;;
    --uninstall) UNINSTALL=1; shift ;;
    --purge) PURGE=1; shift ;;
    --agent) AGENT=1; shift ;;
    --server) AGENT_SERVER="$2"; shift 2 ;;
    --key) AGENT_KEY="$2"; shift 2 ;;
    --allow) AGENT_ALLOW="$2"; shift 2 ;;
    --insecure) AGENT_INSECURE=1; shift ;;
    --allow-physical) AGENT_PHYSICAL=1; shift ;;
    --allow-mssql-dir) AGENT_MSSQL_DIR="$2"; shift 2 ;;
    -h|--help) usage ;;
    *) die "invalid argument: $1 (see --help)" ;;
  esac
done

[[ -z $PG_MAJORS || $PG_MAJORS =~ ^[0-9]+(,[0-9]+)*$ ]] || die "--postgres takes major versions, e.g. --postgres 16 or --postgres 15,16"
[[ $EUID -eq 0 ]] || die "please run as root (sudo)"
command -v systemctl >/dev/null || die "systemd is required"

if [[ $UNINSTALL -eq 1 && $AGENT -eq 1 ]]; then
  info "Uninstalling the DumpCat agent"
  systemctl disable --now dumpcat-agent 2>/dev/null || true
  rm -f "$AGENT_UNIT" "$AGENT_DROPIN" "$AGENT_MSSQL_DROPIN"
  rmdir "$(dirname "$AGENT_DROPIN")" 2>/dev/null || true
  systemctl daemon-reload
  if [[ ! -f $UNIT ]]; then # the binary is shared with the DumpCat server
    rm -f "$BIN_LINK"
    rm -rf "$BIN_DIR" "$DOC_DIR"
  fi
  if [[ $PURGE -eq 1 ]]; then
    rm -f "$AGENT_ENV" "$CONF_DIR/agent.env.example"
    rmdir "$CONF_DIR" 2>/dev/null || true
  else
    info "Keeping $AGENT_ENV (use --purge to delete it)"
  fi
  exit 0
fi

if [[ $UNINSTALL -eq 1 ]]; then
  info "Uninstalling DumpCat"
  systemctl disable --now dumpcat 2>/dev/null || true
  rm -f "$UNIT" "$BIN_LINK"
  rm -rf "$BIN_DIR" "$DOC_DIR"
  systemctl daemon-reload
  if [[ $PURGE -eq 1 ]]; then
    warn "Deleting data: $DATA_DIR and $CONF_DIR (backups on your storages are NOT deleted)"
    rm -rf "$DATA_DIR" "$CONF_DIR"
  else
    info "Keeping data in $DATA_DIR and configuration in $CONF_DIR (use --purge to delete them)"
  fi
  exit 0
fi

# ---------- binary ----------
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "unsupported architecture: $(uname -m)" ;;
esac
if [[ -z "$BINARY" ]]; then
  for cand in "$SCRIPT_DIR/dumpcat-linux-$ARCH" "$SCRIPT_DIR/../dist/dumpcat-linux-$ARCH" "$SCRIPT_DIR/dumpcat" "./dumpcat-linux-$ARCH"; do
    [[ -f "$cand" ]] && { BINARY="$cand"; break; }
  done
fi
[[ -n "$BINARY" && -f "$BINARY" ]] || die "binary dumpcat-linux-$ARCH not found (use --binary /path/to/binary)"

# ---------- agent ----------
# set_env <name> <value>: sets a variable in the agent env file
set_env() {
  local tmp
  tmp="$(mktemp "$AGENT_ENV.XXXXXX")"
  grep -v "^$1=" "$AGENT_ENV" > "$tmp" || true
  printf '%s=%s\n' "$1" "$2" >> "$tmp"
  chmod 0600 "$tmp"
  mv -f "$tmp" "$AGENT_ENV"
}

if [[ $AGENT -eq 1 ]]; then
  if [[ -n $AGENT_SERVER && $AGENT_SERVER != http://* && $AGENT_SERVER != https://* ]]; then
    die "--server must be the DumpCat address, e.g. https://backup.example.com"
  fi
  mkdir -p "$CONF_DIR"
  chmod 0750 "$CONF_DIR"
  if [[ ! -f $AGENT_ENV ]]; then
    [[ -n $AGENT_SERVER ]] || die "--server is required, e.g. --server https://backup.example.com"
    if [[ -z $AGENT_KEY && -t 0 ]]; then
      read -rsp "Agent key (from the Agents page of the web UI): " AGENT_KEY
      echo
    fi
    [[ -n $AGENT_KEY ]] || die "--key is required: the agent key from the Agents page of the web UI"
    info "Creating configuration $AGENT_ENV"
    ( umask 077; printf '%s\n' "# DumpCat agent — see $CONF_DIR/agent.env.example for all options" \
        "# After editing: systemctl restart dumpcat-agent" > "$AGENT_ENV" )
  fi
  [[ -z $AGENT_KEY || $AGENT_KEY == dca_* ]] || warn "agent keys start with dca_ — is this the key from the Agents page?"
  if [[ -n $AGENT_SERVER ]]; then set_env DUMPCAT_AGENT_SERVER "${AGENT_SERVER%/}"; fi
  if [[ -n $AGENT_KEY ]]; then set_env DUMPCAT_AGENT_KEY "$AGENT_KEY"; fi
  if [[ -n $AGENT_ALLOW ]]; then set_env DUMPCAT_AGENT_ALLOW "$AGENT_ALLOW"; fi
  if [[ $AGENT_INSECURE -eq 1 ]]; then set_env DUMPCAT_AGENT_INSECURE 1; fi
  if [[ $AGENT_PHYSICAL -eq 1 ]]; then set_env DUMPCAT_AGENT_ALLOW_PHYSICAL 1; fi
  if [[ -n $AGENT_MSSQL_DIR ]]; then
    [[ $AGENT_MSSQL_DIR == /* && $AGENT_MSSQL_DIR != *[[:space:]]* ]] || die "--allow-mssql-dir takes an absolute path without spaces, e.g. /var/opt/mssql/backup"
    set_env DUMPCAT_AGENT_MSSQL_DIR "${AGENT_MSSQL_DIR%/}"
  fi
  chmod 0600 "$AGENT_ENV"
  grep -q '^DUMPCAT_AGENT_SERVER=.' "$AGENT_ENV" && grep -q '^DUMPCAT_AGENT_KEY=.' "$AGENT_ENV" \
    || die "$AGENT_ENV needs DUMPCAT_AGENT_SERVER and DUMPCAT_AGENT_KEY (use --server and --key)"

  info "Installing the binary to $BIN_PATH"
  mkdir -p "$BIN_DIR"
  chmod 0755 "$BIN_DIR"
  # rename over the old file: a DumpCat server on this host may be running it
  install -m 0755 "$BINARY" "$BIN_PATH.new"
  mv -f "$BIN_PATH.new" "$BIN_PATH"
  ln -sfn "$BIN_PATH" "$BIN_LINK"
  UNIT_SRC=""
  for d in "$SCRIPT_DIR" "$SCRIPT_DIR/../deploy"; do
    [[ -f "$d/agent.env.example" ]] && install -m 0644 "$d/agent.env.example" "$CONF_DIR/agent.env.example"
    [[ -f "$d/dumpcat-agent.service" ]] && { UNIT_SRC="$d/dumpcat-agent.service"; break; }
  done
  [[ -n "$UNIT_SRC" ]] || die "dumpcat-agent.service not found next to install.sh"
  install -m 0644 "$UNIT_SRC" "$AGENT_UNIT"
  if grep -Eq '^DUMPCAT_AGENT_ALLOW_PHYSICAL=(1|true|yes|on)$' "$AGENT_ENV"; then
    # physical backups read the datadir: the agent (and the backup tool it
    # starts) runs as the datadir's owner instead of a throwaway user
    DATADIR="$(grep '^DUMPCAT_AGENT_DATADIR=' "$AGENT_ENV" | tail -1 | cut -d= -f2-)"
    if [[ -z $DATADIR ]] && command -v my_print_defaults >/dev/null; then
      DATADIR="$(my_print_defaults mysqld mariadbd server 2>/dev/null | sed -n 's/^--datadir=//p' | tail -1)"
    fi
    DATADIR="${DATADIR:-/var/lib/mysql}"
    if [[ -d $DATADIR ]]; then
      DB_USER="$(stat -c %U "$DATADIR")"; DB_GROUP="$(stat -c %G "$DATADIR")"
    else
      warn "datadir $DATADIR not found — set DUMPCAT_AGENT_DATADIR in $AGENT_ENV and run this again"
      DB_USER=mysql; DB_GROUP=mysql
    fi
    id "$DB_USER" >/dev/null 2>&1 || die "user $DB_USER (owner of $DATADIR) does not exist"
    [[ $DB_USER != root ]] || warn "$DATADIR belongs to root: the agent cannot read it without capabilities"
    info "Physical backups allowed: the agent runs as $DB_USER (owner of $DATADIR)"
    mkdir -p "$(dirname "$AGENT_DROPIN")"
    cat > "$AGENT_DROPIN" <<EOF
# written by install.sh --allow-physical: mariabackup/xtrabackup read the
# datadir, so the agent runs as its owner instead of a throwaway user
[Service]
DynamicUser=no
User=$DB_USER
Group=$DB_GROUP
MemoryDenyWriteExecute=no
EOF
    if ! command -v mariadb-backup >/dev/null && ! command -v mariabackup >/dev/null && ! command -v xtrabackup >/dev/null; then
      warn "no backup tool found: install mariadb-backup (MariaDB) or percona-xtrabackup (MySQL/Percona, version matching the server)"
    fi
  else
    rm -f "$AGENT_DROPIN"
  fi
  MSSQL_DIR="$(grep '^DUMPCAT_AGENT_MSSQL_DIR=' "$AGENT_ENV" | tail -1 | cut -d= -f2-)"
  if [[ -n $MSSQL_DIR ]]; then
    # SQL Server's backup folder: the agent reads the backups SQL Server
    # writes there and writes the files of restores, as the folder's owner
    [[ $MSSQL_DIR == /* && $MSSQL_DIR != *[[:space:]]* ]] || die "DUMPCAT_AGENT_MSSQL_DIR in $AGENT_ENV must be an absolute path without spaces"
    [[ ! -f $AGENT_DROPIN ]] || die "--allow-physical and --allow-mssql-dir run the agent as different users (the datadir's owner, SQL Server's mssql): use one of them on this host"
    if [[ ! -d $MSSQL_DIR ]]; then
      id mssql >/dev/null 2>&1 || die "$MSSQL_DIR does not exist: create it for SQL Server's service account and run this again"
      info "Creating SQL Server's backup folder $MSSQL_DIR (mssql:mssql, 0770)"
      install -d -o mssql -g mssql -m 0770 "$MSSQL_DIR"
    fi
    DIR_USER="$(stat -c %U "$MSSQL_DIR")"; DIR_GROUP="$(stat -c %G "$MSSQL_DIR")"
    [[ $DIR_USER != root ]] || warn "$MSSQL_DIR belongs to root: SQL Server (mssql) cannot write there — chown mssql:mssql $MSSQL_DIR and run this again"
    info "SQL Server backup folder $MSSQL_DIR: the agent runs as $DIR_USER (its owner) and moves only the dumpcat-* files there"
    mkdir -p "$(dirname "$AGENT_MSSQL_DROPIN")"
    cat > "$AGENT_MSSQL_DROPIN" <<EOF
# written by install.sh --allow-mssql-dir: the agent reads the backups SQL
# Server writes into this folder and writes the files of restores there, so
# it runs as the folder's owner and may write that folder (and only that one)
[Service]
DynamicUser=no
User=$DIR_USER
Group=$DIR_GROUP
ReadWritePaths=$MSSQL_DIR
EOF
  else
    rm -f "$AGENT_MSSQL_DROPIN"
  fi
  systemctl daemon-reload
  systemctl enable dumpcat-agent >/dev/null 2>&1
  STARTED="$(date '+%Y-%m-%d %H:%M:%S')"
  systemctl restart dumpcat-agent
  info "Waiting for the agent to connect"
  CONNECTED=0
  for _ in $(seq 1 15); do
    sleep 1
    systemctl is-active --quiet dumpcat-agent || break
    if journalctl -u dumpcat-agent --since "$STARTED" --no-pager -q 2>/dev/null | grep -q "connected to "; then
      CONNECTED=1
      break
    fi
  done
  systemctl is-active --quiet dumpcat-agent || { journalctl -u dumpcat-agent -n 30 --no-pager; die "the agent failed to start"; }
  echo
  if [[ $CONNECTED -eq 1 ]]; then
    echo "${c_bold}The DumpCat agent is connected.${c_off} It shows as online on the Agents page of the web UI."
  else
    journalctl -u dumpcat-agent -n 10 --no-pager -q || true
    warn "the agent is running but has not connected yet — check the address, the key and that this host can reach the server"
  fi
  echo "Next, in the web UI: Database servers → Add server → Connect: through this agent, with the database"
  echo "address as seen from this host (e.g. 127.0.0.1 port 3306)."
  if [[ -f $AGENT_DROPIN ]]; then
    echo "Physical backups: the Agents page shows the backup tool and whether the datadir is readable."
  fi
  if [[ -f $AGENT_MSSQL_DROPIN ]]; then
    echo "SQL Server: enter $MSSQL_DIR as the server's backup folder (as SQL Server sees it) and leave"
    echo "\"As DumpCat sees it\" empty — the agent moves the files. Allow the instance: --allow 127.0.0.1:1433."
  fi
  echo
  echo "Log: journalctl -u dumpcat-agent -f"
  echo "Configuration: $AGENT_ENV  (after editing: systemctl restart dumpcat-agent)"
  exit 0
fi

# ---------- distro ----------
. /etc/os-release
FAMILY=""
case " ${ID:-} ${ID_LIKE:-} " in
  *" debian "*|*" ubuntu "*) FAMILY=debian ;;
  *" rhel "*|*" fedora "*|*" centos "*|*" rocky "*|*" almalinux "*|*" ol "*) FAMILY=rhel ;;
esac
info "Operating system: ${PRETTY_NAME:-$ID} ($FAMILY, $ARCH)"

pkg_install() {
  if [[ $FAMILY == debian ]]; then
    DEBIAN_FRONTEND=noninteractive apt-get install -y "$@"
  else
    dnf install -y "$@"
  fi
}
have() { command -v "$1" >/dev/null 2>&1; }

detect_server() {
  SERVER_KIND=""; SERVER_VER=""
  local bin=""
  for b in mariadbd mysqld /usr/sbin/mariadbd /usr/sbin/mysqld /usr/libexec/mysqld; do
    if have "$b" || [[ -x "$b" ]]; then bin="$b"; break; fi
  done
  [[ -z "$bin" ]] && return 0
  local v; v="$("$bin" --version 2>/dev/null || true)"
  if grep -qi mariadb <<<"$v"; then SERVER_KIND=mariadb; else SERVER_KIND=mysql; fi
  SERVER_VER="$(grep -oE '[0-9]+\.[0-9]+\.[0-9]+' <<<"$v" | head -1)"
}

install_deps() {
  info "Installing backup tools"
  if [[ $FAMILY == debian ]]; then
    apt-get update -y
    pkg_install ca-certificates curl gnupg lsb-release unzip
  elif [[ $FAMILY == rhel ]]; then
    pkg_install ca-certificates curl unzip
  else
    warn "Unrecognized distro — skipping tool installation; install them manually (see the System page)"
    return 0
  fi

  if ! have rclone; then
    info "Installing rclone (for S3, Google Drive, OneDrive, FTP, SFTP...)"
    if ! curl -fsSL https://rclone.org/install.sh | bash; then
      warn "rclone installation failed — try: $( [[ $FAMILY == debian ]] && echo 'apt install rclone' || echo 'dnf install rclone' )"
    fi
  fi

  detect_server
  if [[ -n "$SERVER_KIND" ]]; then
    info "Detected local server: $SERVER_KIND $SERVER_VER"
  else
    info "No MySQL/MariaDB on this machine — installing clients only, for remote logical backups"
  fi

  # client tools (mysqldump / mysql)
  if ! have mysqldump && ! have mariadb-dump; then
    if [[ $SERVER_KIND == mariadb ]]; then
      pkg_install mariadb-client 2>/dev/null || pkg_install mariadb || warn "could not install mariadb-client"
    elif [[ $FAMILY == debian ]]; then
      pkg_install mysql-client 2>/dev/null || pkg_install default-mysql-client || warn "could not install mysql-client"
    else
      pkg_install mysql 2>/dev/null || pkg_install mariadb || warn "could not install the mysql client"
    fi
  fi

  # physical backup tool matching the local server
  if [[ $SERVER_KIND == mariadb ]] && ! have mariadb-backup && ! have mariabackup; then
    pkg_install mariadb-backup 2>/dev/null || pkg_install MariaDB-backup 2>/dev/null || warn "could not install mariadb-backup (it must match the server version)"
  elif [[ $SERVER_KIND == mysql ]] && ! have xtrabackup; then
    local series="pxb-80" pkg="percona-xtrabackup-80"
    case "$SERVER_VER" in
      8.4.*|9.*) series="pxb-84-lts"; pkg="percona-xtrabackup-84" ;;
      5.7.*) series="pxb-24"; pkg="percona-xtrabackup-24" ;;
    esac
    info "Installing Percona XtraBackup ($pkg) for MySQL $SERVER_VER"
    if [[ $FAMILY == debian ]]; then
      if ! have percona-release; then
        curl -fsSL -o /tmp/percona-release.deb https://repo.percona.com/apt/percona-release_latest.generic_all.deb \
          && pkg_install /tmp/percona-release.deb || warn "could not add the Percona repository"
        rm -f /tmp/percona-release.deb
      fi
      have percona-release && percona-release enable "$series" release && apt-get update -y && pkg_install "$pkg" || warn "installing $pkg failed"
    else
      have percona-release || pkg_install https://repo.percona.com/yum/percona-release-latest.noarch.rpm || warn "could not add the Percona repository"
      have percona-release && percona-release enable "$series" release && pkg_install "$pkg" || warn "installing $pkg failed"
    fi
  fi

  # mydumper (parallel logical backups for large databases)
  if ! have mydumper; then
    if [[ $FAMILY == debian ]]; then
      pkg_install mydumper 2>/dev/null || warn "mydumper is not in the repositories — download the .deb from https://github.com/mydumper/mydumper/releases"
    else
      warn "mydumper: download the .rpm from https://github.com/mydumper/mydumper/releases (optional, for large databases)"
    fi
  fi
  have zstd || pkg_install zstd 2>/dev/null || true

  # PostgreSQL client tools: pg_dump must be at least as new as the server, so one
  # set per major version (--postgres, and the majors of a server on this machine)
  local majors="${PG_MAJORS//,/ }" d m
  for d in /usr/lib/postgresql/*/bin /usr/pgsql-*/bin; do
    [[ -x $d/postgres ]] || continue
    m="$(basename "$(dirname "$d")")"; m="${m#pgsql-}"
    [[ $m =~ ^[0-9]+$ && " $majors " != *" $m "* ]] && majors="$majors $m"
  done
  for m in $majors; do install_pg_client "$m"; done
}

# install_pg_client MAJOR installs pg_dump, pg_restore and pg_dumpall of that
# major version from the PostgreSQL project's repository (the distribution only
# has its own major).
install_pg_client() {
  local major="$1"
  if [[ -x /usr/lib/postgresql/$major/bin/pg_dump || -x /usr/pgsql-$major/bin/pg_dump ]]; then
    info "PostgreSQL $major client tools: already installed"
    return 0
  fi
  info "Installing the PostgreSQL $major client tools (pg_dump, pg_restore, pg_dumpall)"
  if [[ $FAMILY == debian ]]; then
    if ! apt-cache show "postgresql-client-$major" >/dev/null 2>&1; then
      pkg_install postgresql-common && /usr/share/postgresql-common/pgdg/apt.postgresql.org.sh -y \
        || warn "could not add the PostgreSQL repository (apt.postgresql.org)"
    fi
    pkg_install "postgresql-client-$major" || warn "could not install postgresql-client-$major"
  else
    if ! dnf -q list --available "postgresql$major" >/dev/null 2>&1; then
      pkg_install "https://download.postgresql.org/pub/repos/yum/reporpms/EL-$(rpm -E %rhel)-$(uname -m)/pgdg-redhat-repo-latest.noarch.rpm" \
        || warn "could not add the PostgreSQL repository (PGDG)"
      dnf -qy module disable postgresql >/dev/null 2>&1 || true # RHEL 8: hide the distribution's own module
    fi
    pkg_install "postgresql$major" || warn "could not install postgresql$major"
  fi
}

if [[ $INSTALL_DEPS -eq 1 ]]; then
  install_deps
fi

# ---------- install ----------
info "Installing the binary to $BIN_PATH"
systemctl stop dumpcat 2>/dev/null || true
mkdir -p "$BIN_DIR"
chmod 0755 "$BIN_DIR"
install -m 0755 "$BINARY" "$BIN_PATH"
ln -sfn "$BIN_PATH" "$BIN_LINK"   # replaces the binary of older installs

mkdir -p "$CONF_DIR" "$DATA_DIR"
chmod 0700 "$DATA_DIR"
chmod 0750 "$CONF_DIR"
if [[ ! -f "$CONF_DIR/dumpcat.env" ]]; then
  info "Creating configuration $CONF_DIR/dumpcat.env"
  cat > "$CONF_DIR/dumpcat.env" <<EOF
# DumpCat configuration — see $CONF_DIR/dumpcat.env.example for all options
DUMPCAT_LISTEN=${LISTEN_ADDR}:${PORT}
DUMPCAT_DATA_DIR=${DATA_DIR}
DUMPCAT_TLS_AUTO=${TLS_AUTO}
#DUMPCAT_BASE_URL=https://backup.example.com
#DUMPCAT_TRUST_PROXY=1
EOF
  chmod 0640 "$CONF_DIR/dumpcat.env"
else
  info "Keeping the existing configuration $CONF_DIR/dumpcat.env"
fi
for f in dumpcat.env.example nginx-dumpcat.conf; do
  for d in "$SCRIPT_DIR" "$SCRIPT_DIR/../deploy"; do
    [[ -f "$d/$f" ]] && { install -m 0644 "$d/$f" "$CONF_DIR/$f"; break; }
  done
done
for d in "$SCRIPT_DIR" "$SCRIPT_DIR/.."; do
  if [[ -f "$d/README.md" ]]; then
    mkdir -p "$DOC_DIR/docs"
    for f in README.md README.vi.md CONTRIBUTING.md SECURITY.md LICENSE NOTICE THIRD_PARTY_NOTICES.md docs/manual.md docs/manual.vi.md; do
      if [[ -f "$d/$f" ]]; then install -m 0644 "$d/$f" "$DOC_DIR/$f"; fi
    done
    break
  fi
done

UNIT_SRC=""
for d in "$SCRIPT_DIR" "$SCRIPT_DIR/../deploy"; do
  [[ -f "$d/dumpcat.service" ]] && { UNIT_SRC="$d/dumpcat.service"; break; }
done
if [[ -n "$UNIT_SRC" ]]; then
  install -m 0644 "$UNIT_SRC" "$UNIT"
else
  cat > "$UNIT" <<'EOF'
[Unit]
Description=DumpCat - MySQL/MariaDB backup & restore manager
After=network-online.target mysql.service mysqld.service mariadb.service
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=-/etc/dumpcat/dumpcat.env
ExecStart=/opt/dumpcat/dumpcat serve
Restart=on-failure
RestartSec=5
User=root
UMask=0027
LimitNOFILE=65535
KillMode=mixed
TimeoutStopSec=60
PrivateTmp=true
ProtectSystem=full

[Install]
WantedBy=multi-user.target
EOF
fi

systemctl daemon-reload
systemctl enable --now dumpcat
sleep 2
systemctl is-active --quiet dumpcat || { journalctl -u dumpcat -n 30 --no-pager; die "the service failed to start"; }

# firewall hint
if have firewall-cmd && firewall-cmd --state >/dev/null 2>&1; then
  warn "firewalld is active — open the port: firewall-cmd --permanent --add-port=${PORT}/tcp && firewall-cmd --reload"
elif have ufw && ufw status 2>/dev/null | grep -q active; then
  warn "ufw is active — open the port: ufw allow ${PORT}/tcp (better, restrict by IP: ufw allow from <IP> to any port ${PORT})"
fi

SCHEME=http; [[ $TLS_AUTO -eq 1 ]] && SCHEME=https
IP="$(hostname -I 2>/dev/null | awk '{print $1}')"
echo
echo "${c_bold}DumpCat is running:${c_off} ${SCHEME}://${IP:-<server-ip>}:${PORT}/"
if [[ -z "$("$BIN_PATH" user list 2>/dev/null)" ]]; then
  # the service writes the one-time setup token while it starts
  TOKEN=""
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    TOKEN="$("$BIN_PATH" setup-token 2>/dev/null)" && break
    sleep 1
  done
  echo "${c_bold}No account yet:${c_off} open the address above and create the admin account."
  if [[ -n "$TOKEN" ]]; then
    echo "  The setup page asks for this one-time setup token:  ${c_bold}${TOKEN}${c_off}"
  fi
  echo "  Show the token again: sudo dumpcat setup-token  (it is also in: journalctl -u dumpcat)"
  echo "  or create an admin from the command line:  sudo dumpcat user add -u admin -r admin"
fi
echo
echo "Check the installed tools on the System page of the web UI"
echo "Log: journalctl -u dumpcat -f"
echo "Configuration: $CONF_DIR/dumpcat.env  (after editing: systemctl restart dumpcat)"
