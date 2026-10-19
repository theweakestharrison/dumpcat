package store

// wgStored is the WireGuard configuration as kept in the settings table: the
// private key is encrypted with the master key.
type wgStored struct {
	WireGuard
	PrivateKeyEnc string `json:"private_key_enc"`
}

// GetWireGuard returns the built-in WireGuard configuration (off by default).
func (s *Store) GetWireGuard() WireGuard {
	var st wgStored
	if v, ok := s.GetKV("wireguard"); ok {
		fromJSON(v, &st)
	}
	w := st.WireGuard
	w.PrivateKey = s.dec(st.PrivateKeyEnc)
	return w
}

func (s *Store) SaveWireGuard(w WireGuard) error {
	enc, err := s.enc(w.PrivateKey)
	if err != nil {
		return err
	}
	return s.SetKV("wireguard", toJSON(wgStored{WireGuard: w, PrivateKeyEnc: enc}))
}

const wgPeerCols = `id,name,public_key,preshared_key,address,networks,endpoint,created_at,updated_at`

func (s *Store) scanWGPeer(row interface{ Scan(...any) error }) (*WGPeer, error) {
	var p WGPeer
	var psk, nets string
	var ca, ua int64
	if err := row.Scan(&p.ID, &p.Name, &p.PublicKey, &psk, &p.Address, &nets, &p.Endpoint, &ca, &ua); err != nil {
		return nil, notFound(err)
	}
	p.PresharedKey = s.dec(psk)
	p.Networks = []string{}
	fromJSON(nets, &p.Networks)
	p.CreatedAt, p.UpdatedAt = fromTS(ca), fromTS(ua)
	return &p, nil
}

func (s *Store) ListWGPeers() ([]WGPeer, error) {
	rows, err := s.db.Query(`SELECT ` + wgPeerCols + ` FROM wg_peers ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WGPeer
	for rows.Next() {
		p, err := s.scanWGPeer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

func (s *Store) GetWGPeer(id int64) (*WGPeer, error) {
	return s.scanWGPeer(s.db.QueryRow(`SELECT `+wgPeerCols+` FROM wg_peers WHERE id=?`, id))
}

func (s *Store) CreateWGPeer(p *WGPeer) error {
	psk, err := s.enc(p.PresharedKey)
	if err != nil {
		return err
	}
	t := now()
	res, err := s.db.Exec(`INSERT INTO wg_peers(name,public_key,preshared_key,address,networks,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`,
		p.Name, p.PublicKey, psk, p.Address, toJSON(p.Networks), t, t)
	if err != nil {
		return err
	}
	p.ID, _ = res.LastInsertId()
	p.CreatedAt, p.UpdatedAt = fromTS(t), fromTS(t)
	return nil
}

// UpdateWGPeer saves the name and the networks of a peer.
func (s *Store) UpdateWGPeer(p *WGPeer) error {
	_, err := s.db.Exec(`UPDATE wg_peers SET name=?, networks=?, updated_at=? WHERE id=?`, p.Name, toJSON(p.Networks), now(), p.ID)
	return err
}

func (s *Store) SetWGPeerKeys(id int64, pub, psk string) error {
	enc, err := s.enc(psk)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE wg_peers SET public_key=?, preshared_key=?, updated_at=? WHERE id=?`, pub, enc, now(), id)
	return err
}

// SetWGPeerEndpoint remembers where a peer was last seen.
func (s *Store) SetWGPeerEndpoint(id int64, endpoint string) {
	_, _ = s.db.Exec(`UPDATE wg_peers SET endpoint=? WHERE id=?`, endpoint, id)
}

func (s *Store) DeleteWGPeer(id int64) error {
	_, err := s.db.Exec(`DELETE FROM wg_peers WHERE id=?`, id)
	return err
}

// WGPeerServers lists the names of the servers reached through a peer.
func (s *Store) WGPeerServers(id int64) ([]string, error) {
	rows, err := s.db.Query(`SELECT name FROM servers WHERE wg_peer_id=? ORDER BY name`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
