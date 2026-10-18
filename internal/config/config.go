// Package config loads runtime configuration from environment variables,
// an optional env file (KEY=VALUE, systemd EnvironmentFile compatible) and flags.
package config

import (
	"bufio"
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type Config struct {
	Listen     string // address to listen on, e.g. ":6369" or "127.0.0.1:6369"
	DataDir    string // database, keys, logs, staging
	BaseURL    string // public URL used for OAuth callbacks and links in notifications
	TLSCert    string // path to TLS certificate (PEM)
	TLSKey     string // path to TLS private key (PEM)
	TLSAuto    bool   // generate a self-signed certificate in DataDir/tls
	TrustProxy bool   // honor X-Forwarded-For / X-Forwarded-Proto
	MasterKey  string // optional base64 32-byte key overriding DataDir/master.key
	LogFile    string // also write the application log to this file (default for a Windows service: DataDir/dumpcat.log)
	EnvFile    string
}

// WindowsRoot is where the Windows installer keeps configuration and data.
func WindowsRoot() string {
	if d := os.Getenv("ProgramData"); d != "" {
		return filepath.Join(d, "DumpCat")
	}
	return `C:\ProgramData\DumpCat`
}

func DefaultEnvFile() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(WindowsRoot(), "dumpcat.env")
	}
	return "/etc/dumpcat/dumpcat.env"
}

func Default() *Config {
	c := &Config{Listen: ":6369", EnvFile: DefaultEnvFile()}
	if runtime.GOOS == "windows" {
		c.DataDir = filepath.Join(WindowsRoot(), "data")
	} else {
		c.DataDir = "/var/lib/dumpcat"
	}
	return c
}

// LoadEnvFile reads KEY=VALUE pairs and sets them in the process environment
// unless the variable is already set. Missing file is not an error.
func LoadEnvFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// tolerate a UTF-8 BOM (files saved by some Windows editors)
		line := strings.TrimSpace(strings.TrimPrefix(sc.Text(), "\ufeff"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		if _, exists := os.LookupEnv(k); !exists {
			os.Setenv(k, v)
		}
	}
	return sc.Err()
}

func envBool(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// ApplyEnv overrides fields from DUMPCAT_* environment variables.
func (c *Config) ApplyEnv() {
	if v := os.Getenv("DUMPCAT_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("DUMPCAT_DATA_DIR"); v != "" {
		c.DataDir = v
	}
	if v := os.Getenv("DUMPCAT_BASE_URL"); v != "" {
		c.BaseURL = strings.TrimRight(v, "/")
	}
	if v := os.Getenv("DUMPCAT_TLS_CERT"); v != "" {
		c.TLSCert = v
	}
	if v := os.Getenv("DUMPCAT_TLS_KEY"); v != "" {
		c.TLSKey = v
	}
	if v := os.Getenv("DUMPCAT_TLS_AUTO"); v != "" {
		c.TLSAuto = envBool(v)
	}
	if v := os.Getenv("DUMPCAT_TRUST_PROXY"); v != "" {
		c.TrustProxy = envBool(v)
	}
	if v := os.Getenv("DUMPCAT_MASTER_KEY"); v != "" {
		c.MasterKey = v
	}
	if v := os.Getenv("DUMPCAT_LOG_FILE"); v != "" {
		c.LogFile = v
	}
}

func (c *Config) BindFlags(fs *flag.FlagSet) {
	fs.StringVar(&c.Listen, "listen", c.Listen, "listen address (env DUMPCAT_LISTEN)")
	fs.StringVar(&c.DataDir, "data-dir", c.DataDir, "data directory (env DUMPCAT_DATA_DIR)")
	fs.StringVar(&c.BaseURL, "base-url", c.BaseURL, "public base URL, e.g. https://backup.example.com (env DUMPCAT_BASE_URL)")
	fs.StringVar(&c.TLSCert, "tls-cert", c.TLSCert, "TLS certificate file (env DUMPCAT_TLS_CERT)")
	fs.StringVar(&c.TLSKey, "tls-key", c.TLSKey, "TLS key file (env DUMPCAT_TLS_KEY)")
	fs.BoolVar(&c.TLSAuto, "tls-auto", c.TLSAuto, "serve HTTPS with an auto-generated self-signed certificate (env DUMPCAT_TLS_AUTO)")
	fs.BoolVar(&c.TrustProxy, "trust-proxy", c.TrustProxy, "trust X-Forwarded-* headers from a reverse proxy (env DUMPCAT_TRUST_PROXY)")
	fs.StringVar(&c.LogFile, "log-file", c.LogFile, "also write the log to this file, rotated at 20 MB (env DUMPCAT_LOG_FILE)")
}

func (c *Config) Normalize() error {
	abs, err := filepath.Abs(c.DataDir)
	if err != nil {
		return err
	}
	c.DataDir = abs
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	return nil
}

func (c *Config) TLSEnabled() bool {
	return c.TLSAuto || (c.TLSCert != "" && c.TLSKey != "")
}

func (c *Config) Path(parts ...string) string {
	return filepath.Join(append([]string{c.DataDir}, parts...)...)
}
