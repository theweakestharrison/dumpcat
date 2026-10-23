// Package notify delivers backup/restore events to Email, Telegram, Slack,
// Discord and generic webhooks.
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/smtp"
	neturl "net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"dumpcat/internal/storage"
	"dumpcat/internal/store"
)

type Event struct {
	Kind   string            `json:"kind"`   // backup, restore, verify, binlog, stale, binlog_lag, digest, update, test
	Status string            `json:"status"` // success, failed, partial, recovered
	Title  string            `json:"title"`
	Fields map[string]string `json:"fields"`
	Order  []string          `json:"-"`
	Error  string            `json:"error,omitempty"`
	URL    string            `json:"url,omitempty"`
	Time   time.Time         `json:"time"`
	Brand  Brand             `json:"-"` // set by the Notifier from the settings
}

// StatusRecovered ends an alert (stale, binlog_lag): it goes to the channels
// that got the alert, i.e. those that send failures.
const StatusRecovered = "recovered"

func (e *Event) Add(k, v string) {
	if e.Fields == nil {
		e.Fields = map[string]string{}
	}
	if _, ok := e.Fields[k]; !ok {
		e.Order = append(e.Order, k)
	}
	e.Fields[k] = v
}

type TypeDef = storage.TypeDef
type Field = storage.Field

var Types = []TypeDef{
	{Type: "email", Label: "Email (SMTP)", Description: "Send email through an SMTP server (Gmail, Office365, SES, Postfix...)", Fields: []Field{
		{Key: "host", Label: "SMTP host", Type: "text", Required: true, Placeholder: "smtp.gmail.com"},
		{Key: "port", Label: "Port", Type: "number", Default: "587"},
		{Key: "security", Label: "Security", Type: "select", Default: "starttls", Options: []storage.Option{{Value: "starttls", Label: "STARTTLS (587)"}, {Value: "tls", Label: "SSL/TLS (465)"}, {Value: "none", Label: "None"}}},
		{Key: "username", Label: "Username", Type: "text"},
		{Key: "password", Label: "Password / App password", Type: "password", Secret: true},
		{Key: "from", Label: "From", Type: "text", Required: true, Placeholder: "backup@example.com"},
		{Key: "to", Label: "To (comma-separated)", Type: "text", Required: true},
		{Key: "insecure", Label: "Skip TLS certificate verification", Type: "bool"},
		{Key: "digest", Label: "Send the weekly digest", Type: "bool", Help: "A summary of the last 7 days: backups per job, sizes and growth, integrity checks, overdue jobs and storage use (day and hour in Settings)."},
	}},
	{Type: "telegram", Label: "Telegram", Description: "Send through a Telegram bot (create one with @BotFather).", Fields: []Field{
		{Key: "bot_token", Label: "Bot token", Type: "password", Secret: true, Required: true},
		{Key: "chat_id", Label: "Chat ID", Type: "text", Required: true, Help: "User / group ID (group IDs usually start with -100...)"},
		{Key: "thread_id", Label: "Topic (message_thread_id)", Type: "text"},
	}},
	{Type: "slack", Label: "Slack", Description: "Slack Incoming Webhook.", Fields: []Field{
		{Key: "webhook_url", Label: "Webhook URL", Type: "password", Secret: true, Required: true},
	}},
	{Type: "discord", Label: "Discord", Description: "Discord channel webhook.", Fields: []Field{
		{Key: "webhook_url", Label: "Webhook URL", Type: "password", Secret: true, Required: true},
	}},
	{Type: "webhook", Label: "Webhook (JSON)", Description: "POST JSON to any URL, with an optional HMAC-SHA256 signature.", Fields: []Field{
		{Key: "url", Label: "URL", Type: "text", Required: true},
		{Key: "secret", Label: "Secret (HMAC signing)", Type: "password", Secret: true, Help: "Header X-DumpCat-Signature: sha256=<hex>"},
		{Key: "headers", Label: "Extra headers (one Key: Value per line)", Type: "textarea"},
	}},
}

func GetType(t string) (*TypeDef, bool) {
	for i := range Types {
		if Types[i].Type == t {
			return &Types[i], true
		}
	}
	return nil, false
}

func init() { storage.SecretKeysHook = SecretKeys }

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

type Notifier struct {
	Store  *store.Store
	Host   string // the machine DumpCat runs on, shown in every message
	client *http.Client
}

func New(st *store.Store) *Notifier {
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown host"
	}
	return &Notifier{Store: st, Host: host, client: &http.Client{Timeout: 20 * time.Second}}
}

// Send delivers the event asynchronously to matching enabled channels.
func (n *Notifier) Send(ev Event) {
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	chans, err := n.Store.ListChannels()
	if err != nil {
		return
	}
	ev.Brand = n.Brand()
	for _, ch := range chans {
		if !ch.Enabled {
			continue
		}
		if ev.Status == "success" && !ch.OnSuccess {
			continue
		}
		if ev.Status != "success" && !ch.OnFailure { // failures, and the end of an alert
			continue
		}
		go func(ch store.NotifyChannel) {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			if err := n.deliver(ctx, &ch, &ev); err != nil {
				log.Printf("notify %s (%s): %v", ch.Name, ch.Type, err)
			}
		}(ch)
	}
}

// Test sends a test message synchronously.
func (n *Notifier) Test(ctx context.Context, ch *store.NotifyChannel) error {
	ev := Event{Kind: "test", Status: "success", Title: "Notifications work", Time: time.Now(), Brand: n.Brand()}
	ev.Add("Channel", ch.Name)
	ev.Add("Sent from", n.Host)
	ev.Add("Time", ev.Time.Format("2006-01-02 15:04:05"))
	return n.deliver(ctx, ch, &ev)
}

func (n *Notifier) deliver(ctx context.Context, ch *store.NotifyChannel, ev *Event) error {
	c := ch.Config
	switch ch.Type {
	case "email":
		return sendEmail(ctx, c, func(from string, to []string) ([]byte, error) { return ev.emailMessage(from, to, n.Host) })
	case "telegram":
		body := map[string]any{"chat_id": c["chat_id"], "text": ev.telegram(), "parse_mode": "HTML", "disable_web_page_preview": true}
		if t := strings.TrimSpace(c["thread_id"]); t != "" {
			if v, err := strconv.Atoi(t); err == nil {
				body["message_thread_id"] = v
			}
		}
		return n.postJSON(ctx, "https://api.telegram.org/bot"+strings.TrimSpace(c["bot_token"])+"/sendMessage", body, nil, "")
	case "slack":
		return n.postJSON(ctx, c["webhook_url"], ev.slack(n.Host), nil, "")
	case "discord":
		return n.postJSON(ctx, c["webhook_url"], ev.discord(n.Host), nil, "")
	case "webhook":
		hdr := map[string]string{}
		for _, ln := range strings.Split(c["headers"], "\n") {
			k, v, ok := strings.Cut(ln, ":")
			if ok && strings.TrimSpace(k) != "" {
				hdr[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
		return n.postJSON(ctx, c["url"], ev, hdr, c["secret"])
	}
	return fmt.Errorf("unsupported channel type: %s", ch.Type)
}

func (n *Notifier) postJSON(ctx context.Context, url string, v any, headers map[string]string, secret string) error {
	url = strings.TrimSpace(url)
	if url == "" {
		return errors.New("URL is missing")
	}
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "DumpCat")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if secret != "" {
		m := hmac.New(sha256.New, []byte(secret))
		m.Write(body)
		req.Header.Set("X-DumpCat-Signature", "sha256="+hex.EncodeToString(m.Sum(nil)))
	}
	resp, err := n.client.Do(req)
	if err != nil {
		// a webhook URL (Slack, Discord) or a Telegram bot URL is the secret itself: keep it out
		// of the error, which reaches the log and the channel test
		var ue *neturl.Error
		if errors.As(err, &ue) {
			return fmt.Errorf("%s %s://%s/…: %w", ue.Op, req.URL.Scheme, req.URL.Host, ue.Err)
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// SendDigest emails the weekly digest through an email channel.
func (n *Notifier) SendDigest(ctx context.Context, ch *store.NotifyChannel, d *Digest) error {
	if ch.Type != "email" {
		return errors.New("the weekly digest is sent by email only")
	}
	d.Brand = n.Brand()
	return sendEmail(ctx, ch.Config, func(from string, to []string) ([]byte, error) { return d.emailMessage(from, to, n.Host) })
}

// SendDigestAll emails the weekly digest to every channel that opted in.
func (n *Notifier) SendDigestAll(ctx context.Context, d *Digest) {
	for _, ch := range n.DigestChannels() {
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		if err := n.SendDigest(cctx, &ch, d); err != nil {
			log.Printf("weekly digest to %s: %v", ch.Name, err)
		}
		cancel()
	}
}

// DigestChannels returns the enabled email channels that send the weekly digest.
func (n *Notifier) DigestChannels() []store.NotifyChannel {
	chans, err := n.Store.ListChannels()
	if err != nil {
		return nil
	}
	var out []store.NotifyChannel
	for _, ch := range chans {
		if ch.Enabled && ch.Type == "email" && ch.Config["digest"] == "true" {
			out = append(out, ch)
		}
	}
	return out
}

func sendEmail(ctx context.Context, c map[string]string, message func(from string, to []string) ([]byte, error)) error {
	host := strings.TrimSpace(c["host"])
	port := strings.TrimSpace(c["port"])
	if port == "" {
		port = "587"
	}
	from := strings.TrimSpace(c["from"])
	var to []string
	for _, t := range strings.Split(c["to"], ",") {
		if t = strings.TrimSpace(t); t != "" {
			to = append(to, t)
		}
	}
	if host == "" || from == "" || len(to) == 0 {
		return errors.New("host/from/to is missing")
	}
	addr := net.JoinHostPort(host, port)
	tlsCfg := &tls.Config{ServerName: host, InsecureSkipVerify: c["insecure"] == "true", MinVersion: tls.VersionTLS12}
	d := &net.Dialer{Timeout: 20 * time.Second}
	var conn net.Conn
	var err error
	if c["security"] == "tls" {
		conn, err = tls.DialWithDialer(d, "tcp", addr, tlsCfg)
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return err
	}
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	cl, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return err
	}
	defer cl.Close()
	if c["security"] == "starttls" || c["security"] == "" {
		if ok, _ := cl.Extension("STARTTLS"); ok {
			if err := cl.StartTLS(tlsCfg); err != nil {
				return err
			}
		} else if c["security"] == "starttls" {
			return errors.New("the server does not support STARTTLS")
		}
	}
	if u := strings.TrimSpace(c["username"]); u != "" {
		if err := cl.Auth(smtp.PlainAuth("", u, c["password"], host)); err != nil {
			return err
		}
	}
	if err := cl.Mail(from); err != nil {
		return err
	}
	for _, t := range to {
		if err := cl.Rcpt(t); err != nil {
			return err
		}
	}
	w, err := cl.Data()
	if err != nil {
		return err
	}
	msg, merr := message(from, to)
	if merr != nil {
		return merr
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return cl.Quit()
}
