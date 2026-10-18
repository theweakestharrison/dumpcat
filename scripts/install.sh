#!/usr/bin/env bash
# DumpCat installer — MySQL/MariaDB backup & restore manager
#
#   sudo ./install.sh                     # install binary + systemd + backup tools
#   sudo ./install.sh --port 9090         # custom port
#   sudo ./install.sh --no-deps           # do not install backup tools
#   sudo ./install.sh --tls-auto          # serve HTTPS with a self-signed certificate
#   sudo ./install.sh --uninstall [--purge]
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
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

c_green=$'\e[32m'; c_yellow=$'\e[33m'; c_red=$'\e[31m'; c_bold=$'\e[1m'; c_off=$'\e[0m'
info()  { echo "${c_green}==>${c_off} $*"; }
warn()  { echo "${c_yellow}!!${c_off} $*" >&2; }
die()   { echo "${c_red}xx${c_off} $*" >&2; exit 1; }

usage() { sed -n '2,10p' "$0" | sed 's/^# \{0,1\}//'; exit 0; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --port) PORT="$2"; shift 2 ;;
    --listen) LISTEN_ADDR="$2"; shift 2 ;;
    --no-deps) INSTALL_DEPS=0; shift ;;
    --tls-auto) TLS_AUTO=1; shift ;;
    --binary) BINARY="$2"; shift 2 ;;
    --data-dir) DATA_DIR="$2"; shift 2 ;;
    --uninstall) UNINSTALL=1; shift ;;
    --purge) PURGE=1; shift ;;
    -h|--help) usage ;;
    *) die "invalid argument: $1 (see --help)" ;;
  esac
done

[[ $EUID -eq 0 ]] || die "please run as root (sudo)"
command -v systemctl >/dev/null || die "systemd is required"

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
  echo "${c_bold}No account yet:${c_off} open the address above now and create the admin account"
  echo "  (until then, the first visitor becomes the administrator)."
  echo "  or create one from the command line:  sudo dumpcat user add -u admin -r admin"
fi
echo
echo "Check the installed tools on the System page of the web UI"
echo "Log: journalctl -u dumpcat -f"
echo "Configuration: $CONF_DIR/dumpcat.env  (after editing: systemctl restart dumpcat)"
