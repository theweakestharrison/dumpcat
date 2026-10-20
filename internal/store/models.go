package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path"
	"strconv"
	"strings"
	"time"
)

const (
	RoleAdmin    = "admin"
	RoleOperator = "operator"
	RoleViewer   = "viewer"
)

func RoleLevel(role string) int {
	switch role {
	case RoleAdmin:
		return 3
	case RoleOperator:
		return 2
	case RoleViewer:
		return 1
	}
	return 0
}

func ValidRole(role string) bool { return RoleLevel(role) > 0 }

type User struct {
	ID           int64      `json:"id"`
	Username     string     `json:"username"`
	Email        string     `json:"email"`
	Role         string     `json:"role"`
	TOTPEnabled  bool       `json:"totp_enabled"`
	Disabled     bool       `json:"disabled"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	LastLoginAt  *time.Time `json:"last_login_at"`
	PasswordHash string     `json:"-"`
	TOTPSecret   string     `json:"-"` // plaintext base32 (encrypted at rest)
}

type Session struct {
	ID         string // sha256 hex of the cookie token
	UserID     int64
	CSRF       string
	IP         string
	UserAgent  string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
}

type APIToken struct {
	ID         int64      `json:"id"`
	UserID     int64      `json:"user_id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	Access     string     `json:"access"` // TokenRead, TokenFull
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	ExpiresAt  *time.Time `json:"expires_at"`
	Hash       string     `json:"-"`
}

const (
	TokenRead = "read" // looks only: no change, no backup download, whatever the user's role
	TokenFull = "full" // everything the user's role allows
)

// ReadOnly tells whether the token may only look (any value but full: fail closed).
func (t *APIToken) ReadOnly() bool { return t.Access != TokenFull }

type Server struct {
	ID          int64     `json:"id"`
	Engine      string    `json:"engine"` // mysql (MySQL, MariaDB, Percona), postgres
	Name        string    `json:"name"`
	Host        string    `json:"host"`
	Port        int       `json:"port"`
	Socket      string    `json:"socket"`
	Username    string    `json:"username"`
	Password    string    `json:"-"`
	TLSMode     string    `json:"tls_mode"` // "", disabled, required, verify
	CACert      string    `json:"ca_cert"`
	Flavor      string    `json:"flavor"` // MySQL: auto, mysql, mariadb, percona
	Version     string    `json:"version"`
	Datadir     string    `json:"datadir"`
	ServiceName string    `json:"service_name"`
	OSUser      string    `json:"os_user"`
	Notes       string    `json:"notes"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	HasPassword bool      `json:"has_password"`
	// Optional credentials for restores (backup user can stay read-only).
	RestoreUsername    string `json:"restore_username"`
	RestorePassword    string `json:"-"`
	HasRestorePassword bool   `json:"has_restore_password"`
	// AgentID: reach the server through this agent (it connects out to
	// DumpCat); Host/Port/Socket are then as seen from the agent.
	AgentID *int64 `json:"agent_id"`
	// WGPeerID: reach the server through DumpCat's WireGuard; Host is an
	// address routed to this peer (its VPN address or a network behind it).
	WGPeerID *int64 `json:"wg_peer_id"`
}

const (
	EngineMySQL    = "mysql"
	EnginePostgres = "postgres"
)

// EngineName is the server's engine ("mysql" for servers saved before engines existed).
func (s *Server) EngineName() string {
	if s.Engine == "" {
		return EngineMySQL
	}
	return s.Engine
}

// DefaultPort is the engine's usual port, used when the server has none.
func (s *Server) DefaultPort() int {
	if s.EngineName() == EnginePostgres {
		return 5432
	}
	return 3306
}

// TunnelEndpoint resolves a target behind an agent to a local address (set
// by the agent manager at startup).
var TunnelEndpoint func(agentID int64, network, addr string) (host string, port int, err error)

// WGEndpoint resolves an address behind a WireGuard peer to a local address
// (set at startup).
var WGEndpoint func(peerID int64, addr string) (host string, port int, err error)

// Tunneled tells whether DumpCat reaches the server through an agent or
// WireGuard, i.e. it does not run on the database host.
func (s *Server) Tunneled() bool {
	return (s.AgentID != nil && *s.AgentID > 0) || (s.WGPeerID != nil && *s.WGPeerID > 0)
}

// Route identifies the network the server's address is seen from:
// "" (DumpCat itself), "agent:<id>" or "wg:<id>".
func (s *Server) Route() string {
	switch {
	case s.AgentID != nil && *s.AgentID > 0:
		return fmt.Sprintf("agent:%d", *s.AgentID)
	case s.WGPeerID != nil && *s.WGPeerID > 0:
		return fmt.Sprintf("wg:%d", *s.WGPeerID)
	}
	return ""
}

// Addr is where to connect: the server's own address (host/port, or a unix
// socket when socket is set) or the local end of its agent tunnel.
func (s *Server) Addr() (host string, port int, socket string, err error) {
	if s.AgentID != nil && *s.AgentID > 0 {
		network, addr := s.AgentTarget()
		if TunnelEndpoint == nil {
			return "", 0, "", errors.New("agent connections are not available")
		}
		host, port, err := TunnelEndpoint(*s.AgentID, network, addr)
		if err != nil {
			return "", 0, "", fmt.Errorf("server %s: %w", s.Name, err)
		}
		return host, port, "", nil
	}
	if s.WGPeerID != nil && *s.WGPeerID > 0 {
		if WGEndpoint == nil {
			return "", 0, "", errors.New("WireGuard connections are not available")
		}
		port := s.Port
		if port == 0 {
			port = s.DefaultPort()
		}
		host, port, err := WGEndpoint(*s.WGPeerID, net.JoinHostPort(s.Host, strconv.Itoa(port)))
		if err != nil {
			return "", 0, "", fmt.Errorf("server %s: %w", s.Name, err)
		}
		return host, port, "", nil
	}
	if s.Socket != "" && (s.Host == "" || s.Host == "localhost") {
		return s.Host, 0, s.Socket, nil
	}
	host = s.Host
	if host == "" || host == "localhost" {
		host = "127.0.0.1"
	}
	port = s.Port
	if port == 0 {
		port = s.DefaultPort()
	}
	return host, port, "", nil
}

// AgentTarget is the address the agent dials for this server.
func (s *Server) AgentTarget() (network, addr string) {
	port := s.Port
	if port == 0 {
		port = s.DefaultPort()
	}
	if s.Socket != "" && (s.Host == "" || s.Host == "localhost") {
		if s.EngineName() == EnginePostgres && !strings.Contains(path.Base(s.Socket), ".s.PGSQL.") {
			// PostgreSQL names the socket directory: the socket is .s.PGSQL.<port> inside it
			return "unix", path.Join(s.Socket, ".s.PGSQL."+strconv.Itoa(port))
		}
		return "unix", s.Socket
	}
	host := s.Host
	if host == "" || host == "localhost" {
		host = "127.0.0.1"
	}
	return "tcp", net.JoinHostPort(host, strconv.Itoa(port))
}

// WGPeer is a database host (or a gateway to a network of them) that joins
// DumpCat's built-in WireGuard.
type WGPeer struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	PublicKey    string    `json:"public_key"`
	PresharedKey string    `json:"-"`
	Address      string    `json:"address"`       // VPN address of the peer
	Networks     []string  `json:"networks"`      // more networks routed to it (CIDR)
	Endpoint     string    `json:"last_endpoint"` // where it was last seen (ip:port)
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// WireGuard is the configuration of the built-in WireGuard endpoint.
type WireGuard struct {
	Enabled    bool   `json:"enabled"`
	ListenPort int    `json:"listen_port"`
	Address    string `json:"address"`  // DumpCat's VPN address and subnet, e.g. 10.66.0.1/24
	Endpoint   string `json:"endpoint"` // host[:port] the peers connect to
	PrivateKey string `json:"-"`
	PublicKey  string `json:"public_key"`
}

// Agent is a database host that connects out to DumpCat.
type Agent struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	KeyPrefix  string     `json:"key_prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	LastSeenAt *time.Time `json:"last_seen_at"`
	LastIP     string     `json:"last_ip"`
	Version    string     `json:"version"`
	Hostname   string     `json:"hostname"`
	OS         string     `json:"os"`
	KeyHash    string     `json:"-"`
}

// ForRestore returns a copy using the restore credentials when configured.
func (s *Server) ForRestore() *Server {
	c := *s
	if s.RestoreUsername != "" {
		c.Username, c.Password = s.RestoreUsername, s.RestorePassword
	}
	return &c
}

type Storage struct {
	ID          int64             `json:"id"`
	Name        string            `json:"name"`
	Type        string            `json:"type"`
	Config      map[string]string `json:"-"` // plaintext in memory, encrypted at rest
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
	LastTestAt  *time.Time        `json:"last_test_at"`
	LastTestOK  *bool             `json:"last_test_ok"`
	LastTestMsg string            `json:"last_test_msg"`
}

type Retention struct {
	KeepLast    int `json:"keep_last"`
	KeepDays    int `json:"keep_days"`
	KeepDaily   int `json:"keep_daily"`
	KeepWeekly  int `json:"keep_weekly"`
	KeepMonthly int `json:"keep_monthly"`
}

func (r Retention) IsZero() bool {
	return r.KeepLast == 0 && r.KeepDays == 0 && r.KeepDaily == 0 && r.KeepWeekly == 0 && r.KeepMonthly == 0
}

type JobOptions struct {
	// logical
	SingleFile     bool   `json:"single_file"`
	Routines       bool   `json:"routines"`
	Triggers       bool   `json:"triggers"`
	Events         bool   `json:"events"`
	IncludeUsers   bool   `json:"include_users"`
	RecordBinlog   bool   `json:"record_binlog"`
	GTIDPurged     string `json:"gtid_purged"`
	ChunkRows      int    `json:"chunk_rows"`
	LongQueryGuard int    `json:"long_query_guard"`
	LockMode       string `json:"lock_mode"` // mydumper --sync-thread-lock-mode (empty = AUTO)
	// physical
	FTWRLWaitTimeout int  `json:"ftwrl_wait_timeout"`
	SlaveInfo        bool `json:"slave_info"`
	SafeSlaveBackup  bool `json:"safe_slave_backup"`
	// general
	LowPriority  bool   `json:"low_priority"`
	StreamUpload bool   `json:"stream_upload"`
	VerifyAfter  bool   `json:"verify_after"` // kept in line with VerifyMode for older API clients
	VerifyMode   string `json:"verify_mode"`  // after each backup: off, one (the cheapest copy), all
	BwLimit      string `json:"bwlimit"`
	ExtraArgs    string `json:"extra_args"`
	PreCommand   string `json:"pre_command"`
	PostCommand  string `json:"post_command"`
	// continuous log archiving (point-in-time recovery): MySQL binlogs,
	// PostgreSQL WAL (the JSON names predate PostgreSQL)
	LogArchive       bool `json:"binlog_archive"`
	LogRotateMinutes int  `json:"binlog_rotate_minutes"` // close the current file after N minutes (0 = never)
	// monitoring
	MaxAgeHours int `json:"max_age_hours"` // alert when the newest successful backup is older (0 = from the schedule)
}

const (
	VerifyOff = "off"
	VerifyOne = "one"
	VerifyAll = "all"
)

// Verify tells how a job's backups are checked after they are written.
func (o JobOptions) Verify() string {
	switch o.VerifyMode {
	case VerifyOff, VerifyOne, VerifyAll:
		return o.VerifyMode
	}
	if o.VerifyAfter { // jobs from before verify_mode
		return VerifyAll
	}
	return VerifyOff
}

type JobTarget struct {
	StorageID   int64      `json:"storage_id"`
	StorageName string     `json:"storage_name"`
	StorageType string     `json:"storage_type"`
	Retention   *Retention `json:"retention"` // nil = use job retention
}

type Job struct {
	ID               int64       `json:"id"`
	UUID             string      `json:"uuid"`
	Name             string      `json:"name"`
	ServerID         int64       `json:"server_id"`
	ServerName       string      `json:"server_name"`
	Method           string      `json:"method"`  // mysqldump, mydumper, physical
	DBMode           string      `json:"db_mode"` // all, include, exclude
	Databases        []string    `json:"databases"`
	ExcludeTables    []string    `json:"exclude_tables"`
	Schedule         string      `json:"schedule"`
	IncrSchedule     string      `json:"incr_schedule"`
	Compression      string      `json:"compression"` // zstd, gzip, none
	CompressionLevel int         `json:"compression_level"`
	Encrypt          bool        `json:"encrypt"`
	Passphrase       string      `json:"-"`
	Threads          int         `json:"threads"`
	Options          JobOptions  `json:"options"`
	Retention        Retention   `json:"retention"`
	Targets          []JobTarget `json:"targets"`
	Enabled          bool        `json:"enabled"`
	NotifySuccess    bool        `json:"notify_success"`
	NotifyFailure    bool        `json:"notify_failure"`
	TimeoutMinutes   int         `json:"timeout_minutes"`
	CreatedAt        time.Time   `json:"created_at"`
	UpdatedAt        time.Time   `json:"updated_at"`
	HasPassphrase    bool        `json:"has_passphrase"`
}

const (
	MethodMysqldump = "mysqldump"
	MethodMydumper  = "mydumper"
	MethodPhysical  = "physical"
	MethodPgDump    = "pg_dump"
	MethodPgBase    = "pg_basebackup"

	TypeFull        = "full"
	TypeIncremental = "incremental"

	StatusQueued   = "queued"
	StatusRunning  = "running"
	StatusSuccess  = "success"
	StatusPartial  = "partial"
	StatusFailed   = "failed"
	StatusCanceled = "canceled"
	StatusDeleted  = "deleted"

	LocUploading = "uploading"
	LocOK        = "ok"
	LocFailed    = "failed"
)

type BackupFile struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
	Kind     string `json:"kind"` // sql, users, tar, xbstream
	Database string `json:"database,omitempty"`
	RawSize  int64  `json:"raw_size,omitempty"`
}

type DBEntry struct {
	Name      string `json:"name"`
	Charset   string `json:"charset"`
	Collation string `json:"collation"`
	SizeBytes int64  `json:"size_bytes"`
	File      string `json:"file,omitempty"`
	Tables    int    `json:"tables"`
	// binlog position of this database's own snapshot (per-database dumps)
	BinlogFile string `json:"binlog_file,omitempty"`
	BinlogPos  int64  `json:"binlog_pos,omitempty"`
	// rows in the dump, counted while it was written (logical backups)
	Rows int64 `json:"rows,omitempty"`
}

type Run struct {
	ID            int64          `json:"id"`
	UUID          string         `json:"uuid"`
	JobID         *int64         `json:"job_id"`
	JobName       string         `json:"job_name"`
	JobUUID       string         `json:"job_uuid"`
	JobSlug       string         `json:"job_slug"`
	ServerID      *int64         `json:"server_id"`
	ServerName    string         `json:"server_name"`
	Method        string         `json:"method"`
	Type          string         `json:"type"`
	ParentID      *int64         `json:"parent_id"`
	BaseID        *int64         `json:"base_id"`
	ParentUUID    string         `json:"parent_uuid"`
	BaseUUID      string         `json:"base_uuid"`
	Status        string         `json:"status"`
	Trigger       string         `json:"trigger"`
	TriggeredBy   string         `json:"triggered_by"`
	StartedAt     time.Time      `json:"started_at"`
	FinishedAt    *time.Time     `json:"finished_at"`
	SizeBytes     int64          `json:"size_bytes"`
	RawBytes      int64          `json:"raw_bytes"`
	Files         []BackupFile   `json:"files"`
	Databases     []DBEntry      `json:"databases"`
	Meta          map[string]any `json:"meta"`
	Error         string         `json:"error"`
	Compression   string         `json:"compression"`
	Encrypted     bool           `json:"encrypted"`
	Passphrase    string         `json:"-"`
	DirName       string         `json:"dir_name"`
	VerifyStatus  string         `json:"verify_status"`
	VerifiedAt    *time.Time     `json:"verified_at"`
	VerifyMsg     string         `json:"verify_msg"`
	DrillStatus   string         `json:"drill_status"` // last restore drill: passed, failed
	DrilledAt     *time.Time     `json:"drilled_at"`
	DrillMsg      string         `json:"drill_msg"`
	Locations     []RunLocation  `json:"locations"`
	HasPassphrase bool           `json:"has_passphrase"`
}

func (r *Run) RelDir() string { return r.JobSlug + "/" + r.DirName }

type RunLocation struct {
	ID          int64     `json:"id"`
	RunID       int64     `json:"run_id"`
	StorageID   *int64    `json:"storage_id"`
	StorageName string    `json:"storage_name"`
	StorageType string    `json:"storage_type"`
	Path        string    `json:"path"`
	Status      string    `json:"status"`
	Error       string    `json:"error"`
	SizeBytes   int64     `json:"size_bytes"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// LogFile is one archived log file of a job's server: a MySQL binary log or
// a PostgreSQL WAL segment (table binlog_files, which predates PostgreSQL).
type LogFile struct {
	ID          int64         `json:"id"`
	JobID       *int64        `json:"job_id"`
	JobUUID     string        `json:"job_uuid"`
	JobName     string        `json:"job_name"`
	ServerID    *int64        `json:"server_id"`
	ServerName  string        `json:"server_name"`
	FileName    string        `json:"file_name"` // name on the server: mysql-bin.000012, 000000010000000000000003
	Seq         int64         `json:"seq"`
	FirstTS     time.Time     `json:"first_ts,omitzero"` // first event (zero: the file holds no timed event)
	LastTS      time.Time     `json:"last_ts,omitzero"`  // last event
	SizeBytes   int64         `json:"size_bytes"`
	StoredBytes int64         `json:"stored_bytes"`
	SHA256      string        `json:"sha256"` // of the stored (compressed/encrypted) file
	RawSHA256   string        `json:"raw_sha256"`
	Compression string        `json:"compression"`
	Encrypted   bool          `json:"encrypted"`
	Passphrase  string        `json:"-"`
	Path        string        `json:"path"` // relative path on every storage
	CreatedAt   time.Time     `json:"created_at"`
	Locations   []LogLocation `json:"locations"`
	// Meta holds what the engine knows of the file (WAL: timeline, LSN range,
	// restore points, system identifier).
	Meta map[string]any `json:"meta,omitempty"`
}

type LogLocation struct {
	ID          int64     `json:"id"`
	FileID      int64     `json:"binlog_id"`
	StorageID   *int64    `json:"storage_id"`
	StorageName string    `json:"storage_name"`
	StorageType string    `json:"storage_type"`
	Status      string    `json:"status"`
	Error       string    `json:"error"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// OKOn returns the location on storageID when the file is stored there.
func (b *LogFile) OKOn(storageID int64) *LogLocation {
	for i := range b.Locations {
		l := &b.Locations[i]
		if l.StorageID != nil && *l.StorageID == storageID && l.Status == LocOK {
			return l
		}
	}
	return nil
}

type Restore struct {
	ID               int64           `json:"id"`
	RunID            *int64          `json:"run_id"`
	RunUUID          string          `json:"run_uuid"`
	JobName          string          `json:"job_name"`
	Kind             string          `json:"kind"`
	TargetServerID   *int64          `json:"target_server_id"`
	TargetServerName string          `json:"target_server_name"`
	Options          json.RawMessage `json:"options"`
	Status           string          `json:"status"`
	StartedAt        time.Time       `json:"started_at"`
	FinishedAt       *time.Time      `json:"finished_at"`
	Error            string          `json:"error"`
	UserID           *int64          `json:"user_id"`
	Username         string          `json:"username"`
}

type NotifyChannel struct {
	ID        int64             `json:"id"`
	Name      string            `json:"name"`
	Type      string            `json:"type"`
	Config    map[string]string `json:"-"`
	Enabled   bool              `json:"enabled"`
	OnSuccess bool              `json:"on_success"`
	OnFailure bool              `json:"on_failure"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
}

type AuditEntry struct {
	ID       int64     `json:"id"`
	TS       time.Time `json:"ts"`
	UserID   *int64    `json:"user_id"`
	Username string    `json:"username"`
	Action   string    `json:"action"`
	Target   string    `json:"target"`
	Details  string    `json:"details"`
	IP       string    `json:"ip"`
}

type Settings struct {
	Timezone         string            `json:"timezone"`
	MaxConcurrent    int               `json:"max_concurrent"`
	StagingDir       string            `json:"staging_dir"`
	HistoryDays      int               `json:"history_days"`
	BaseURL          string            `json:"base_url"`
	ToolPaths        map[string]string `json:"tool_paths"`
	SessionHours     int               `json:"session_hours"`
	RcloneFlags      string            `json:"rclone_flags"`
	DefaultThreads   int               `json:"default_threads"`
	Require2FAAdmins bool              `json:"require_2fa_admins"`
	UpdateURL        string            `json:"update_url"`        // release server (overrides DUMPCAT_UPDATE_URL)
	UpdateAutoCheck  bool              `json:"update_auto_check"` // look for a new version once a day
	// monitoring
	FreshnessAlerts   bool   `json:"freshness_alerts"`    // alert when a job has no recent successful backup
	StaleGraceMinutes int    `json:"stale_grace_minutes"` // added to the window expected from a job's schedule
	BinlogLagMinutes  int    `json:"binlog_lag_minutes"`  // alert when binlog archiving lags this much (0 = never)
	MetricsAccess     string `json:"metrics_access"`      // GET /metrics: "" (off), "token" or "localhost"
	DigestWeekday     int    `json:"digest_weekday"`      // weekly digest: day (0 = Sunday)
	DigestHour        int    `json:"digest_hour"`         // and hour, in the time zone above
	// search engines may index the web UI (off: robots.txt, X-Robots-Tag and the page say noindex)
	AllowIndexing bool `json:"allow_indexing"`
	// branding: the name in the UI, page titles and notifications ("" = DumpCat); the logo is stored apart
	InstanceName string `json:"instance_name"`
}

const (
	MetricsOff       = ""
	MetricsToken     = "token"
	MetricsLocalhost = "localhost"
)

func DefaultSettings() Settings {
	return Settings{
		MaxConcurrent:     2,
		HistoryDays:       90,
		SessionHours:      12,
		DefaultThreads:    4,
		ToolPaths:         map[string]string{},
		UpdateAutoCheck:   true,
		FreshnessAlerts:   true,
		StaleGraceMinutes: 60,
		BinlogLagMinutes:  30,
		DigestWeekday:     1,
		DigestHour:        8,
	}
}

// Manifest is written next to every backup (manifest.json) so that the
// catalog can be rebuilt from storage alone (disaster recovery / import).
type Manifest struct {
	Format      string         `json:"format"`
	AppVersion  string         `json:"app_version"`
	Engine      string         `json:"engine,omitempty"` // mysql when missing (backups before 1.5.0)
	UUID        string         `json:"uuid"`
	JobUUID     string         `json:"job_uuid"`
	JobName     string         `json:"job_name"`
	JobSlug     string         `json:"job_slug"`
	DirName     string         `json:"dir_name"`
	Method      string         `json:"method"`
	Type        string         `json:"type"`
	ParentUUID  string         `json:"parent_uuid,omitempty"`
	BaseUUID    string         `json:"base_uuid,omitempty"`
	StartedAt   time.Time      `json:"started_at"`
	FinishedAt  time.Time      `json:"finished_at"`
	Server      ManifestServer `json:"server"`
	Compression string         `json:"compression"`
	Encryption  string         `json:"encryption"`
	Databases   []DBEntry      `json:"databases"`
	Files       []BackupFile   `json:"files"`
	RawBytes    int64          `json:"raw_bytes"`
	SizeBytes   int64          `json:"size_bytes"`
	Meta        map[string]any `json:"meta"`
	Tables      TableStats     `json:"tables,omitempty"` // rows per table (logical backups)
}

type ManifestServer struct {
	Name    string `json:"name"`
	Host    string `json:"host"`
	Port    int    `json:"port"`
	Version string `json:"version"`
	Flavor  string `json:"flavor"`
}

const ManifestFormat = "dumpcat-backup/1"

// LegacyManifestFormat is carried by backups made before the project was renamed to DumpCat.
const LegacyManifestFormat = "myvault-backup/1"
