package store

import (
	"encoding/json"
	"strings"
)

// DefaultBrand names DumpCat until an admin gives the instance a name (Settings → Branding).
const DefaultBrand = "DumpCat"

// Brand is the instance name shown in the UI and in notifications.
func (st Settings) Brand() string {
	if n := strings.TrimSpace(st.InstanceName); n != "" {
		return n
	}
	return DefaultBrand
}

// Logo is the image an admin uploaded in place of the cat.
type Logo struct {
	Type string `json:"type"` // image/png, image/svg+xml or image/webp, as the upload was checked
	Data []byte `json:"data"`
	Hash string `json:"hash"` // versions its URL
}

const logoKV = "branding_logo"

// GetLogo returns the uploaded logo (false: the default cat).
func (s *Store) GetLogo() (*Logo, bool) {
	v, ok := s.GetKV(logoKV)
	if !ok || v == "" {
		return nil, false
	}
	var l Logo
	if json.Unmarshal([]byte(v), &l) != nil || len(l.Data) == 0 {
		return nil, false
	}
	return &l, true
}

func (s *Store) SetLogo(l *Logo) error { return s.SetKV(logoKV, toJSON(l)) }

func (s *Store) DeleteLogo() error {
	_, err := s.db.Exec(`DELETE FROM settings WHERE key=?`, logoKV)
	return err
}
