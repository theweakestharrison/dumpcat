package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"dumpcat/internal/auth"
)

type oauthProvider struct {
	AuthURL         string
	TokenURL        string
	Scopes          func(cfg map[string]string) []string
	DefaultRedirect string
	Params          []oauth2.AuthCodeOption
}

var oauthProviders = map[string]oauthProvider{
	"gdrive": {
		AuthURL:  "https://accounts.google.com/o/oauth2/auth",
		TokenURL: "https://oauth2.googleapis.com/token",
		Scopes: func(cfg map[string]string) []string {
			s := cfg["scope"]
			if s == "" {
				s = "drive"
			}
			return []string{"https://www.googleapis.com/auth/" + s}
		},
		DefaultRedirect: "http://127.0.0.1:53682/",
		Params:          []oauth2.AuthCodeOption{oauth2.AccessTypeOffline, oauth2.SetAuthURLParam("prompt", "consent")},
	},
	"onedrive": {
		AuthURL:  "https://login.microsoftonline.com/common/oauth2/v2.0/authorize",
		TokenURL: "https://login.microsoftonline.com/common/oauth2/v2.0/token",
		Scopes: func(cfg map[string]string) []string {
			return []string{"Files.Read", "Files.ReadWrite", "Files.Read.All", "Files.ReadWrite.All", "Sites.Read.All", "offline_access"}
		},
		DefaultRedirect: "http://localhost:53682/",
		Params:          []oauth2.AuthCodeOption{oauth2.SetAuthURLParam("prompt", "select_account")},
	},
	"dropbox": {
		AuthURL:  "https://www.dropbox.com/oauth2/authorize",
		TokenURL: "https://api.dropboxapi.com/oauth2/token",
		Scopes: func(cfg map[string]string) []string {
			return []string{"files.metadata.write", "files.content.write", "files.content.read", "account_info.read"}
		},
		DefaultRedirect: "http://localhost:53682/",
		Params:          []oauth2.AuthCodeOption{oauth2.SetAuthURLParam("token_access_type", "offline")},
	},
}

type oauthState struct {
	typ      string
	conf     *oauth2.Config
	verifier string
	created  time.Time
	userID   int64
	result   *OAuthResult
	err      string
}

type OAuthResult struct {
	Token     string `json:"token"`
	DriveID   string `json:"drive_id,omitempty"`
	DriveType string `json:"drive_type,omitempty"`
	Account   string `json:"account,omitempty"`
}

type OAuthManager struct {
	mu     sync.Mutex
	states map[string]*oauthState
}

func NewOAuthManager() *OAuthManager {
	return &OAuthManager{states: map[string]*oauthState{}}
}

func (m *OAuthManager) gc() {
	for k, s := range m.states {
		if time.Since(s.created) > 30*time.Minute {
			delete(m.states, k)
		}
	}
}

// Start creates an authorization URL. redirect may be empty to use the
// provider's loopback default (manual "paste the redirected URL" flow).
func (m *OAuthManager) Start(typ string, cfg map[string]string, redirect string, userID int64) (authURL, state, usedRedirect string, err error) {
	p, ok := oauthProviders[typ]
	if !ok {
		return "", "", "", fmt.Errorf("type %s does not support OAuth", typ)
	}
	clientID := strings.TrimSpace(cfg["client_id"])
	if clientID == "" {
		return "", "", "", errors.New("enter the Client ID (and Client Secret, if any) before authorizing in the browser, or run rclone authorize and paste the token")
	}
	if redirect == "" {
		redirect = p.DefaultRedirect
	}
	conf := &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: strings.TrimSpace(cfg["client_secret"]),
		Endpoint:     oauth2.Endpoint{AuthURL: p.AuthURL, TokenURL: p.TokenURL},
		RedirectURL:  redirect,
		Scopes:       p.Scopes(cfg),
	}
	state = auth.RandomToken(24)
	verifier := oauth2.GenerateVerifier()
	opts := append([]oauth2.AuthCodeOption{}, p.Params...)
	opts = append(opts, oauth2.S256ChallengeOption(verifier))
	authURL = conf.AuthCodeURL(state, opts...)
	m.mu.Lock()
	m.gc()
	m.states[state] = &oauthState{typ: typ, conf: conf, verifier: verifier, created: time.Now(), userID: userID}
	m.mu.Unlock()
	return authURL, state, redirect, nil
}

// ParseCallback accepts a full redirected URL, a query string or a bare code.
func ParseCallback(input string) (code, state string, err error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", "", errors.New("empty input")
	}
	if strings.Contains(input, "code=") || strings.Contains(input, "error=") {
		q := input
		if i := strings.Index(q, "?"); i >= 0 {
			q = q[i+1:]
		}
		if i := strings.Index(q, "#"); i >= 0 {
			q = q[:i]
		}
		v, perr := url.ParseQuery(q)
		if perr != nil {
			return "", "", perr
		}
		if e := v.Get("error"); e != "" {
			return "", "", fmt.Errorf("the provider returned an error: %s %s", e, v.Get("error_description"))
		}
		return v.Get("code"), v.Get("state"), nil
	}
	return input, "", nil
}

// Exchange trades an authorization code for a token (rclone JSON format).
func (m *OAuthManager) Exchange(ctx context.Context, state, code string, userID int64) (*OAuthResult, error) {
	m.mu.Lock()
	st, ok := m.states[state]
	m.mu.Unlock()
	if !ok {
		return nil, errors.New("the authorization session has expired — start the authorization again")
	}
	if st.userID != userID {
		return nil, errors.New("the authorization session belongs to a different user")
	}
	if code == "" {
		return nil, errors.New("no authorization code")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tok, err := st.conf.Exchange(ctx, code, oauth2.VerifierOption(st.verifier))
	if err != nil {
		m.setErr(state, err.Error())
		return nil, fmt.Errorf("exchanging the code for a token failed: %w", err)
	}
	res := &OAuthResult{Token: RcloneToken(tok)}
	if st.typ == "onedrive" {
		if id, dt, acct, err := DiscoverOneDrive(ctx, tok.AccessToken); err == nil {
			res.DriveID, res.DriveType, res.Account = id, dt, acct
		}
	}
	m.mu.Lock()
	st.result = res
	m.mu.Unlock()
	return res, nil
}

func (m *OAuthManager) setErr(state, msg string) {
	m.mu.Lock()
	if st, ok := m.states[state]; ok {
		st.err = msg
	}
	m.mu.Unlock()
}

// Result returns the outcome of a callback-based flow (polled by the UI).
func (m *OAuthManager) Result(state string, userID int64) (*OAuthResult, string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.states[state]
	if !ok || st.userID != userID {
		return nil, "authorization session not found", true
	}
	if st.result != nil {
		return st.result, "", true
	}
	if st.err != "" {
		return nil, st.err, true
	}
	return nil, "", false
}

// RcloneToken serializes a token the way rclone stores it in its config.
func RcloneToken(t *oauth2.Token) string {
	v := struct {
		AccessToken  string    `json:"access_token"`
		TokenType    string    `json:"token_type,omitempty"`
		RefreshToken string    `json:"refresh_token,omitempty"`
		Expiry       time.Time `json:"expiry,omitempty"`
	}{t.AccessToken, t.TokenType, t.RefreshToken, t.Expiry}
	if v.TokenType == "" {
		v.TokenType = "Bearer"
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// DiscoverOneDrive finds the default drive id/type for a OneDrive token.
func DiscoverOneDrive(ctx context.Context, accessToken string) (id, driveType, account string, err error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://graph.microsoft.com/v1.0/me/drive", nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", "", "", fmt.Errorf("graph: HTTP %d", resp.StatusCode)
	}
	var d struct {
		ID        string `json:"id"`
		DriveType string `json:"driveType"`
		Owner     struct {
			User struct {
				DisplayName string `json:"displayName"`
			} `json:"user"`
		} `json:"owner"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return "", "", "", err
	}
	return d.ID, d.DriveType, d.Owner.User.DisplayName, nil
}

// AccessTokenFrom extracts access_token from a rclone token JSON.
func AccessTokenFrom(tokenJSON string) string {
	var t struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.Unmarshal([]byte(NormalizeToken(tokenJSON)), &t)
	return t.AccessToken
}
