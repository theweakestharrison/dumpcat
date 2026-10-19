package storage

import (
	"fmt"
	"strings"
)

type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

type Field struct {
	Key         string              `json:"key"`
	Label       string              `json:"label"`
	Type        string              `json:"type"` // text, password, number, select, textarea, bool
	Required    bool                `json:"required"`
	Default     string              `json:"default,omitempty"`
	Options     []Option            `json:"options,omitempty"`
	Help        string              `json:"help,omitempty"`
	Placeholder string              `json:"placeholder,omitempty"`
	Secret      bool                `json:"secret"`
	ShowIf      map[string][]string `json:"show_if,omitempty"`
}

type TypeDef struct {
	Type        string  `json:"type"`
	Label       string  `json:"label"`
	Description string  `json:"description"`
	OAuth       bool    `json:"oauth"`
	Fields      []Field `json:"fields"`
}

func opts(pairs ...string) []Option {
	var out []Option
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, Option{Value: pairs[i], Label: pairs[i+1]})
	}
	return out
}

var folderField = Field{Key: "folder", Label: "Base folder", Type: "text", Placeholder: "mysql-backups", Help: "Folder on the storage that holds the backups (created if missing)."}

var Types = []TypeDef{
	{
		Type: "local", Label: "Local directory / NFS", Description: "Stored on the server's disk (or a mounted NFS/SMB share). Fastest restores.",
		Fields: []Field{
			{Key: "path", Label: "Path", Type: "text", Required: true, Placeholder: "/backup/mysql", Help: "Absolute path on the server running DumpCat."},
		},
	},
	{
		Type: "s3", Label: "Amazon S3 / S3-compatible", Description: "AWS S3, Cloudflare R2, MinIO, Wasabi, DigitalOcean Spaces, Backblaze S3, Ceph...",
		Fields: []Field{
			{Key: "provider", Label: "Provider", Type: "select", Default: "AWS", Options: opts(
				"AWS", "Amazon Web Services", "Cloudflare", "Cloudflare R2", "Minio", "MinIO", "Wasabi", "Wasabi",
				"DigitalOcean", "DigitalOcean Spaces", "Ceph", "Ceph", "Alibaba", "Alibaba OSS", "IDrive", "IDrive e2",
				"Linode", "Linode/Akamai", "Scaleway", "Scaleway", "Other", "Other (S3-compatible)")},
			{Key: "access_key_id", Label: "Access Key ID", Type: "text", Help: "Leave blank to use an IAM role / environment variables."},
			{Key: "secret_access_key", Label: "Secret Access Key", Type: "password", Secret: true},
			{Key: "region", Label: "Region", Type: "text", Placeholder: "ap-southeast-1"},
			{Key: "endpoint", Label: "Endpoint", Type: "text", Placeholder: "https://s3.example.com", Help: "Required for providers other than AWS."},
			{Key: "bucket", Label: "Bucket", Type: "text", Required: true},
			{Key: "prefix", Label: "Prefix (folder in the bucket)", Type: "text", Placeholder: "mysql"},
			{Key: "storage_class", Label: "Storage class", Type: "select", Options: opts("", "Default", "STANDARD", "STANDARD", "STANDARD_IA", "STANDARD_IA", "ONEZONE_IA", "ONEZONE_IA", "INTELLIGENT_TIERING", "INTELLIGENT_TIERING", "GLACIER_IR", "GLACIER_IR")},
			{Key: "sse", Label: "Server-side encryption", Type: "select", Options: opts("", "None", "AES256", "AES256", "aws:kms", "aws:kms")},
			{Key: "force_path_style", Label: "Path-style URL (MinIO/Ceph)", Type: "bool"},
			{Key: "no_check_bucket", Label: "Don't check/create the bucket", Type: "bool", Help: "Enable if the key is not allowed to create buckets."},
		},
	},
	{
		Type: "gdrive", Label: "Google Drive", Description: "Personal Google Drive or a Shared Drive (Workspace). Supports OAuth or a service account.", OAuth: true,
		Fields: []Field{
			{Key: "auth_mode", Label: "Authentication", Type: "select", Default: "oauth", Options: opts("oauth", "OAuth (Google account)", "service_account", "Service Account (JSON)")},
			{Key: "client_id", Label: "OAuth Client ID", Type: "text", ShowIf: map[string][]string{"auth_mode": {"oauth"}}, Help: "Created in the Google Cloud Console (Desktop app or Web type). Required for browser authorization."},
			{Key: "client_secret", Label: "OAuth Client Secret", Type: "password", Secret: true, ShowIf: map[string][]string{"auth_mode": {"oauth"}}},
			{Key: "token", Label: "Token (JSON)", Type: "textarea", Secret: true, ShowIf: map[string][]string{"auth_mode": {"oauth"}}, Help: "Click \"Authorize with Google\" to fetch it automatically, or paste the output of: rclone authorize \"drive\""},
			{Key: "service_account_credentials", Label: "Service Account JSON", Type: "textarea", Secret: true, ShowIf: map[string][]string{"auth_mode": {"service_account"}}, Help: "Service accounts have no storage quota of their own: use a Shared Drive or share a folder with the service account."},
			{Key: "scope", Label: "Scope", Type: "select", Default: "drive", Options: opts("drive", "drive (full access)", "drive.file", "drive.file (only files created by the app)")},
			{Key: "team_drive", Label: "Shared Drive ID", Type: "text", Help: "ID of the Shared Drive (if used)."},
			{Key: "root_folder_id", Label: "Root folder ID", Type: "text", Help: "Optional: ID of the root folder."},
			folderField,
		},
	},
	{
		Type: "onedrive", Label: "Microsoft OneDrive / SharePoint", Description: "OneDrive Personal, OneDrive for Business, SharePoint document library.", OAuth: true,
		Fields: []Field{
			{Key: "client_id", Label: "Client ID (Azure App)", Type: "text", Help: "App registration in Microsoft Entra ID. Required for browser authorization."},
			{Key: "client_secret", Label: "Client Secret", Type: "password", Secret: true},
			{Key: "token", Label: "Token (JSON)", Type: "textarea", Secret: true, Help: "Click \"Authorize with Microsoft\" or paste the output of: rclone authorize \"onedrive\""},
			{Key: "drive_type", Label: "Drive type", Type: "select", Default: "personal", Options: opts("personal", "OneDrive Personal", "business", "OneDrive for Business", "documentLibrary", "SharePoint Document Library")},
			{Key: "drive_id", Label: "Drive ID", Type: "text", Help: "Detected automatically after authorization if left blank."},
			{Key: "region", Label: "Region", Type: "select", Default: "global", Options: opts("global", "Global (default)", "us", "US Government", "de", "Germany", "cn", "China (21Vianet)")},
			folderField,
		},
	},
	{
		Type: "ftp", Label: "FTP / FTPS", Description: "Plain FTP server or FTP over TLS.",
		Fields: []Field{
			{Key: "host", Label: "Host", Type: "text", Required: true},
			{Key: "port", Label: "Port", Type: "number", Default: "21"},
			{Key: "user", Label: "Username", Type: "text", Required: true},
			{Key: "pass", Label: "Password", Type: "password", Secret: true},
			{Key: "tls", Label: "Encryption", Type: "select", Default: "none", Options: opts("none", "None (plain FTP)", "explicit", "Explicit TLS (FTPES)", "implicit", "Implicit TLS (FTPS, port 990)")},
			{Key: "no_check_certificate", Label: "Skip TLS certificate verification", Type: "bool"},
			folderField,
		},
	},
	{
		Type: "sftp", Label: "SFTP (SSH)", Description: "Store on another server over SSH/SFTP.",
		Fields: []Field{
			{Key: "host", Label: "Host", Type: "text", Required: true},
			{Key: "port", Label: "Port", Type: "number", Default: "22"},
			{Key: "user", Label: "Username", Type: "text", Required: true},
			{Key: "pass", Label: "Password", Type: "password", Secret: true, Help: "Leave blank when using a private key."},
			{Key: "key_pem", Label: "Private key (PEM)", Type: "textarea", Secret: true, Placeholder: "-----BEGIN OPENSSH PRIVATE KEY-----"},
			{Key: "key_file_pass", Label: "Key passphrase", Type: "password", Secret: true},
			folderField,
		},
	},
	{
		Type: "webdav", Label: "WebDAV / Nextcloud", Description: "Nextcloud, ownCloud, SharePoint WebDAV, NAS (Synology/QNAP)...",
		Fields: []Field{
			{Key: "url", Label: "URL", Type: "text", Required: true, Placeholder: "https://cloud.example.com/remote.php/dav/files/user"},
			{Key: "vendor", Label: "Vendor", Type: "select", Default: "other", Options: opts("nextcloud", "Nextcloud", "owncloud", "ownCloud", "sharepoint", "SharePoint", "rclone", "rclone serve", "other", "Other")},
			{Key: "user", Label: "Username", Type: "text"},
			{Key: "pass", Label: "Password", Type: "password", Secret: true},
			folderField,
		},
	},
	{
		Type: "b2", Label: "Backblaze B2", Description: "Backblaze B2 native API.",
		Fields: []Field{
			{Key: "account", Label: "Key ID / Account ID", Type: "text", Required: true},
			{Key: "key", Label: "Application Key", Type: "password", Secret: true, Required: true},
			{Key: "bucket", Label: "Bucket", Type: "text", Required: true},
			{Key: "prefix", Label: "Prefix", Type: "text"},
		},
	},
	{
		Type: "azureblob", Label: "Azure Blob Storage", Description: "Microsoft Azure Blob container.",
		Fields: []Field{
			{Key: "account", Label: "Storage account", Type: "text"},
			{Key: "key", Label: "Account key", Type: "password", Secret: true},
			{Key: "sas_url", Label: "SAS URL (instead of a key)", Type: "password", Secret: true},
			{Key: "container", Label: "Container", Type: "text", Required: true},
			{Key: "prefix", Label: "Prefix", Type: "text"},
		},
	},
	{
		Type: "dropbox", Label: "Dropbox", Description: "Dropbox Personal / Business.", OAuth: true,
		Fields: []Field{
			{Key: "client_id", Label: "App key", Type: "text"},
			{Key: "client_secret", Label: "App secret", Type: "password", Secret: true},
			{Key: "token", Label: "Token (JSON)", Type: "textarea", Secret: true, Help: "Click \"Authorize with Dropbox\" or paste the output of: rclone authorize \"dropbox\""},
			folderField,
		},
	},
	{
		Type: "rclone", Label: "rclone (custom)", Description: "Any backend rclone supports (70+): Mega, pCloud, Box, Swift, HDFS, SMB... Paste the configuration as key = value lines.",
		Fields: []Field{
			{Key: "config", Label: "rclone configuration", Type: "textarea", Secret: true, Required: true, Placeholder: "type = smb\nhost = 10.0.0.5\nuser = backup\npass = <obscured with rclone obscure>", Help: "Contents of one section of rclone.conf (without the [name] line)."},
			{Key: "path", Label: "Path", Type: "text", Placeholder: "share/mysql-backups"},
		},
	},
}

func GetType(t string) (*TypeDef, bool) {
	for i := range Types {
		if Types[i].Type == t {
			return &Types[i], true
		}
	}
	return nil, false
}

// Validate checks required fields.
func Validate(t string, cfg map[string]string) error {
	td, ok := GetType(t)
	if !ok {
		return fmt.Errorf("unsupported storage type: %s", t)
	}
	for _, f := range td.Fields {
		if !f.Required || !fieldVisible(f, cfg) {
			continue
		}
		if strings.TrimSpace(cfg[f.Key]) == "" {
			return fmt.Errorf("missing required field: %s", f.Label)
		}
	}
	if t == "gdrive" {
		mode := cfg["auth_mode"]
		if mode == "service_account" && strings.TrimSpace(cfg["service_account_credentials"]) == "" {
			return fmt.Errorf("Service Account JSON is required")
		}
		if mode != "service_account" && strings.TrimSpace(cfg["token"]) == "" {
			return fmt.Errorf("no token yet: authorize with Google or paste a token")
		}
	}
	if (t == "onedrive" || t == "dropbox") && strings.TrimSpace(cfg["token"]) == "" {
		return fmt.Errorf("no token yet: authorize or paste a token")
	}
	return nil
}

func fieldVisible(f Field, cfg map[string]string) bool {
	for k, vals := range f.ShowIf {
		v := cfg[k]
		if v == "" {
			// fall back to the default of the controlling field
			v = defaultOf(k)
		}
		ok := false
		for _, a := range vals {
			if a == v {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

func defaultOf(key string) string {
	for _, t := range Types {
		for _, f := range t.Fields {
			if f.Key == key && f.Default != "" {
				return f.Default
			}
		}
	}
	return ""
}

// SecretKeysHook lets other packages (notify) provide secret keys for their types.
var SecretKeysHook func(t string) map[string]bool

func secretKeysOf(t string) map[string]bool {
	if SecretKeysHook != nil {
		return SecretKeysHook(t)
	}
	return nil
}

// SecretKeys returns the set of secret field keys for a type.
func SecretKeys(t string) map[string]bool {
	out := map[string]bool{}
	if td, ok := GetType(t); ok {
		for _, f := range td.Fields {
			if f.Secret {
				out[f.Key] = true
			}
		}
	}
	return out
}

// Masked returns a copy of cfg with secret values replaced by a marker.
const SecretMask = "__SECRET_UNCHANGED__"

func Masked(t string, cfg map[string]string) map[string]any {
	sec := SecretKeys(t)
	out := map[string]any{}
	for k, v := range cfg {
		if sec[k] {
			if v != "" {
				out[k] = SecretMask
			} else {
				out[k] = ""
			}
			continue
		}
		out[k] = v
	}
	return out
}

// destinationKeys identify where credentials are sent; changing one of them
// requires re-entering saved secrets.
var destinationKeys = []string{"host", "port", "user", "url", "endpoint", "account", "provider", "username"}

// RequireSecretsOnMove rejects keeping saved secrets (mask) when the
// destination changed, so stored credentials cannot be redirected.
func RequireSecretsOnMove(t string, incoming, existing map[string]string) error {
	moved := false
	for _, k := range destinationKeys {
		if strings.TrimSpace(incoming[k]) != strings.TrimSpace(existing[k]) {
			moved = true
		}
	}
	// switching certificate checks off lets someone on the path pose as the server: same rule
	for _, k := range []string{"no_check_certificate", "insecure", "skip_verify", "insecure_skip_verify"} {
		if on(incoming[k]) && !on(existing[k]) {
			moved = true
		}
	}
	if !moved {
		return nil
	}
	sec := SecretKeys(t)
	if len(sec) == 0 {
		sec = secretKeysOf(t)
	}
	for k := range sec {
		if incoming[k] == SecretMask && existing[k] != "" {
			return fmt.Errorf("re-enter the password / secret key when changing host/URL/endpoint/user or turning off certificate checks")
		}
	}
	return nil
}

// MergeSecrets keeps existing secret values when the client sends the mask.
func MergeSecrets(t string, incoming, existing map[string]string) map[string]string {
	sec := SecretKeys(t)
	out := map[string]string{}
	for k, v := range incoming {
		if sec[k] && v == SecretMask {
			out[k] = existing[k]
			continue
		}
		out[k] = v
	}
	return out
}

// on reports a checkbox value that is set.
func on(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes", "on":
		return true
	}
	return false
}
