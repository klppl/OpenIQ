package garmin

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Garmin SSO / OAuth1 endpoints (reverse-engineered from the Connect IQ app).
const (
	ssoBase       = "https://sso.garmin.com/sso"
	ssoEmbed      = ssoBase + "/embed"
	ssoSignin     = ssoBase + "/signin"
	oauthPreauth  = "https://connectapi.garmin.com/oauth-service/oauth/preauthorized"
	oauthConsumer = "https://connectapi.garmin.com/oauth-service/oauth/tokens/consumer"

	uaBrowser = "GCM-iOS-5.7.2.1"
	uaOAuth   = "com.garmin.android.apps.connectmobile"

	// The public Garmin mobile OAuth1 consumer (widely documented; used only to
	// turn the SSO ticket into a user OAuth1 token). Overridable via env.
	defaultLoginConsumerKey    = "fc3e99d2-118c-44b8-8ae3-03370dde24c0"
	defaultLoginConsumerSecret = "E08WAR897WEy2knn7aFBrvegVAf0AFdWBBF"
)

var (
	// ErrMFA is returned when the account has two-factor auth enabled.
	ErrMFA = errors.New("account requires MFA/2FA, which this tool does not support")
	// ErrNotLoggedIn is returned when credentials are needed but no session exists.
	ErrNotLoggedIn = errors.New("not logged in")
	// ErrNoStoreCreds is returned when the Connect IQ consumer secret is not configured.
	ErrNoStoreCreds = errors.New("Connect IQ store credentials not set: define GARMIN_CIQ_CONSUMER_KEY and GARMIN_CIQ_CONSUMER_SECRET (see secrets.env.example)")
)

type tokens struct {
	OAuth1Token  string `json:"oauth1_token"`  // login-consumer scoped
	OAuth1Secret string `json:"oauth1_secret"` //
	CIQToken     string `json:"ciq_token"`     // CIQ-consumer scoped (for appstore)
	CIQSecret    string `json:"ciq_secret"`    //
}

// Auth manages the Garmin login session and signs Connect IQ store requests.
type Auth struct {
	mu        sync.Mutex
	loginMu   sync.Mutex // serializes auto-login attempts
	tok       tokens
	tokenFile string
	http      *http.Client

	loginKey, loginSecret string // public login consumer
	ciqKey, ciqSecret     string // private Connect IQ store consumer (from env)

	email, password string // optional headless auto-login (from env)
}

func NewAuth(tokenFile string) *Auth {
	a := &Auth{
		tokenFile:   tokenFile,
		http:        &http.Client{Timeout: 30 * time.Second},
		loginKey:    envOr("GARMIN_LOGIN_CONSUMER_KEY", defaultLoginConsumerKey),
		loginSecret: envOr("GARMIN_LOGIN_CONSUMER_SECRET", defaultLoginConsumerSecret),
		ciqKey:      os.Getenv("GARMIN_CIQ_CONSUMER_KEY"),
		ciqSecret:   os.Getenv("GARMIN_CIQ_CONSUMER_SECRET"),
		email:       os.Getenv("GARMIN_EMAIL"),
		password:    os.Getenv("GARMIN_PASSWORD"),
	}
	a.load()
	return a
}

// HasStoreCreds reports whether the Connect IQ consumer secret is configured.
func (a *Auth) HasStoreCreds() bool { return a.ciqKey != "" && a.ciqSecret != "" }

// AutoLogin reports whether headless email/password login is configured (env).
func (a *Auth) AutoLogin() bool { return a.email != "" && a.password != "" }

// EnsureLogin logs in using the configured env credentials if not already
// signed in. No-op (returns nil) if a session already exists; returns
// ErrNotLoggedIn if no env credentials are configured.
func (a *Auth) EnsureLogin() error {
	if a.LoggedIn() {
		return nil
	}
	if !a.AutoLogin() {
		return ErrNotLoggedIn
	}
	a.loginMu.Lock()
	defer a.loginMu.Unlock()
	if a.LoggedIn() { // re-check after acquiring the lock
		return nil
	}
	return a.Login(a.email, a.password)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func (a *Auth) load() {
	if b, err := os.ReadFile(a.tokenFile); err == nil {
		_ = json.Unmarshal(b, &a.tok)
	}
}

func (a *Auth) save() {
	b, _ := json.MarshalIndent(a.tok, "", "  ")
	_ = os.WriteFile(a.tokenFile, b, 0o600)
}

func (a *Auth) LoggedIn() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.tok.OAuth1Token != ""
}

func (a *Auth) Logout() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tok = tokens{}
	_ = os.Remove(a.tokenFile)
}

// Login runs the full Garmin SSO flow and persists the resulting OAuth1 token.
func (a *Auth) Login(email, password string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 30 * time.Second}

	embedParams := url.Values{"id": {"gauth-widget"}, "embedWidget": {"true"}, "gauthHost": {ssoBase}}
	signinParams := url.Values{
		"id": {"gauth-widget"}, "embedWidget": {"true"},
		"gauthHost": {ssoEmbed}, "service": {ssoEmbed}, "source": {ssoEmbed},
		"redirectAfterAccountLoginUrl":    {ssoEmbed},
		"redirectAfterAccountCreationUrl": {ssoEmbed},
	}

	if _, err := a.doGet(client, ssoEmbed+"?"+embedParams.Encode(), uaBrowser, ""); err != nil {
		return fmt.Errorf("sso embed: %w", err)
	}
	signinURL := ssoSignin + "?" + signinParams.Encode()
	body, err := a.doGet(client, signinURL, uaBrowser, ssoEmbed)
	if err != nil {
		return fmt.Errorf("sso signin GET: %w", err)
	}
	csrf := reCSRF.FindStringSubmatch(body)
	if csrf == nil {
		return errors.New("could not find CSRF token (Garmin may have changed the login page)")
	}

	form := url.Values{"username": {email}, "password": {password}, "embed": {"true"}, "_csrf": {csrf[1]}}
	req, _ := http.NewRequest("POST", signinURL, strings.NewReader(form.Encode()))
	req.Header.Set("User-Agent", uaBrowser)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", signinURL)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("sso signin POST: %w", err)
	}
	loginBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	page := string(loginBody)

	if strings.Contains(strings.ToLower(page), "mfa") && reTicket.FindString(page) == "" {
		return ErrMFA
	}
	m := reTicket.FindStringSubmatch(page)
	if m == nil {
		return errors.New("login failed: wrong email/password, or Garmin blocked the request")
	}

	t1, s1, err := a.preauthorized(client, m[1])
	if err != nil {
		return fmt.Errorf("oauth1 preauthorized: %w", err)
	}
	a.tok.OAuth1Token, a.tok.OAuth1Secret = t1, s1

	// Re-issue the token under the Connect IQ consumer for store access.
	if err := a.reissueCIQ(); err != nil {
		return fmt.Errorf("ciq token exchange: %w", err)
	}
	a.save()
	return nil
}

func (a *Auth) doGet(client *http.Client, u, ua, referer string) (string, error) {
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", ua)
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return string(b), nil
}

func (a *Auth) preauthorized(client *http.Client, ticket string) (token, secret string, err error) {
	params := url.Values{
		"ticket":             {ticket},
		"login-url":          {ssoEmbed},
		"accepts-mfa-tokens": {"true"},
	}
	req, _ := http.NewRequest("GET", oauthPreauth+"?"+params.Encode(), nil)
	req.Header.Set("User-Agent", uaOAuth)
	req.Header.Set("Authorization", oauth1Sign(a.loginKey, a.loginSecret, "GET", oauthPreauth, params, "", ""))
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	vals, _ := url.ParseQuery(string(b))
	token, secret = vals.Get("oauth_token"), vals.Get("oauth_token_secret")
	if token == "" || secret == "" {
		return "", "", errors.New("no oauth_token in response")
	}
	return token, secret, nil
}

// reissueCIQ swaps the login-consumer token for a Connect-IQ-consumer token.
// Caller holds a.mu.
func (a *Auth) reissueCIQ() error {
	if !a.HasStoreCreds() {
		return ErrNoStoreCreds
	}
	// Signed with the CIQ consumer but presenting the user's login token.
	req, _ := http.NewRequest("GET", oauthConsumer, nil)
	req.Header.Set("User-Agent", uaOAuth)
	req.Header.Set("Authorization", oauth1Sign(a.ciqKey, a.ciqSecret, "GET", oauthConsumer, nil, a.tok.OAuth1Token, a.tok.OAuth1Secret))
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var r struct {
		Token  string `json:"token"`
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(b, &r); err != nil || r.Token == "" {
		return fmt.Errorf("bad tokens/consumer response: %s", strings.TrimSpace(string(b)))
	}
	a.tok.CIQToken, a.tok.CIQSecret = r.Token, r.Secret
	return nil
}

// SignStore returns an OAuth1 Authorization header for a Connect IQ store request,
// signed under the CIQ consumer with the user's CIQ-scoped token.
func (a *Auth) SignStore(method, baseURL string, params url.Values) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.tok.OAuth1Token == "" {
		return "", ErrNotLoggedIn
	}
	if !a.HasStoreCreds() {
		return "", ErrNoStoreCreds
	}
	if a.tok.CIQToken == "" {
		if err := a.reissueCIQ(); err != nil {
			return "", err
		}
		a.save()
	}
	return oauth1Sign(a.ciqKey, a.ciqSecret, method, baseURL, params, a.tok.CIQToken, a.tok.CIQSecret), nil
}

// ---- OAuth1 (HMAC-SHA1) signing ----

func oauth1Sign(ck, cs, method, baseURL string, extra url.Values, token, tokenSecret string) string {
	op := map[string]string{
		"oauth_consumer_key":     ck,
		"oauth_nonce":            nonce(),
		"oauth_signature_method": "HMAC-SHA1",
		"oauth_timestamp":        strconv.FormatInt(time.Now().Unix(), 10),
		"oauth_version":          "1.0",
	}
	if token != "" {
		op["oauth_token"] = token
	}
	var pairs []string
	for k, v := range op {
		pairs = append(pairs, pctEncode(k)+"="+pctEncode(v))
	}
	for k, vs := range extra {
		for _, v := range vs {
			pairs = append(pairs, pctEncode(k)+"="+pctEncode(v))
		}
	}
	sort.Strings(pairs)
	base := method + "&" + pctEncode(baseURL) + "&" + pctEncode(strings.Join(pairs, "&"))
	key := pctEncode(cs) + "&" + pctEncode(tokenSecret)

	mac := hmac.New(sha1.New, []byte(key))
	mac.Write([]byte(base))
	op["oauth_signature"] = base64.StdEncoding.EncodeToString(mac.Sum(nil))

	var hp []string
	for k, v := range op {
		hp = append(hp, pctEncode(k)+`="`+pctEncode(v)+`"`)
	}
	sort.Strings(hp)
	return "OAuth " + strings.Join(hp, ", ")
}

func nonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// pctEncode is RFC3986 percent-encoding as required by OAuth1.
func pctEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

var (
	reCSRF   = regexp.MustCompile(`name="_csrf"\s+value="([^"]+)"`)
	reTicket = regexp.MustCompile(`embed\?ticket=([^"]+)"`)
)
