package notify

import (
	"strings"

	"dumpcat/internal/store"
)

// Brand is how messages name this instance (Settings → Branding): its name and, in emails, its
// logo when that is a PNG (mail clients do not show SVG or WebP images).
type Brand struct {
	Name string // "" = DumpCat
	Logo []byte // PNG; nil = the cat
}

func (b Brand) name() string {
	if n := strings.TrimSpace(b.Name); n != "" {
		return n
	}
	return store.DefaultBrand
}

// EmailLogo is the image attached to emails as cid:logo@dumpcat.
func (b Brand) EmailLogo() []byte {
	if len(b.Logo) > 0 {
		return b.Logo
	}
	return logoPNG
}

// subject starts an email subject with a custom instance name, so that mail from several
// instances can be told apart.
func (b Brand) subject(s string) string {
	if n := b.name(); n != store.DefaultBrand {
		return "[" + n + "] " + s
	}
	return s
}

// discordName is the webhook's user name: Discord refuses names with some words and characters.
func (b Brand) discordName() string {
	n := b.name()
	l := strings.ToLower(n)
	if len([]rune(n)) > 80 || strings.ContainsAny(n, "@#:`") || strings.Contains(l, "discord") || strings.Contains(l, "clyde") ||
		l == "everyone" || l == "here" {
		return store.DefaultBrand
	}
	return n
}

// Brand reads the instance name and logo from the settings.
func (n *Notifier) Brand() Brand {
	if n.Store == nil {
		return Brand{}
	}
	b := Brand{Name: n.Store.GetSettings().InstanceName}
	if l, ok := n.Store.GetLogo(); ok && l.Type == "image/png" {
		b.Logo = l.Data
	}
	return b
}
