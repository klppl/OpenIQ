package main

import (
	"bufio"
	"embed"
	"errors"
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"openiq/garmin"
)

//go:embed templates/*.html
var tmplFS embed.FS

var tmpl = template.Must(template.New("").Funcs(template.FuncMap{
	"stars": func(r *float64) string {
		if r == nil {
			return ""
		}
		n := int(*r + 0.5)
		return strings.Repeat("★", n) + strings.Repeat("☆", 5-n)
	},
	"commas": func(n *int) string {
		if n == nil {
			return "0"
		}
		s := strconv.Itoa(*n)
		var out []string
		for len(s) > 3 {
			out = append([]string{s[len(s)-3:]}, out...)
			s = s[:len(s)-3]
		}
		out = append([]string{s}, out...)
		return strings.Join(out, ",")
	},
	"img": func(id string) string {
		if id == "" {
			return ""
		}
		return garmin.ImageBase + "/icons/" + id
	},
	"shot": func(id string) string { return garmin.ImageBase + "/screenshots/" + id },
	"trim": func(s string, n int) string {
		if len(s) <= n {
			return s
		}
		return s[:n] + "…"
	},
}).ParseFS(tmplFS, "templates/*.html"))

type server struct {
	auth    *garmin.Auth
	client  *garmin.Client
	devMu   sync.Mutex
	devices []garmin.Device
	devAt   time.Time
}

func main() {
	// Load secrets.env (gitignored) into the environment before reading creds.
	loadEnvFile("secrets.env")
	if v := os.Getenv("GARMIN_SECRETS_FILE"); v != "" {
		loadEnvFile(v)
	}

	tokenFile := os.Getenv("GARMIN_TOKEN_FILE")
	if tokenFile == "" {
		dir, err := os.UserConfigDir()
		if err != nil || dir == "" {
			dir = "."
		}
		tokenFile = filepath.Join(dir, "garmin-store-tokens.json")
	}
	_ = os.MkdirAll(filepath.Dir(tokenFile), 0o700)

	auth := garmin.NewAuth(tokenFile)
	if !auth.HasStoreCreds() {
		log.Printf("WARNING: GARMIN_CIQ_CONSUMER_KEY/SECRET not set — the model list works, " +
			"but logging in and browsing will fail. See secrets.env.example.")
	}
	// Headless mode: if GARMIN_EMAIL/PASSWORD are set, sign in up front.
	if auth.AutoLogin() && !auth.LoggedIn() {
		if err := auth.EnsureLogin(); err != nil {
			log.Printf("WARNING: auto-login failed: %v (will retry on first request)", err)
		} else {
			log.Printf("auto-login OK for %s", os.Getenv("GARMIN_EMAIL"))
		}
	}
	s := &server{auth: auth, client: garmin.NewClient(auth)}

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleHome)
	mux.HandleFunc("/login", s.handleLogin)
	mux.HandleFunc("/logout", s.handleLogout)
	mux.HandleFunc("/device/", s.handleDevice)
	mux.HandleFunc("/app/", s.handleApp)

	addr := ":8087"
	if v := os.Getenv("ADDR"); v != "" {
		addr = v
	}
	log.Printf("OpenIQ — Connect IQ store browser → http://localhost%s  (tokens: %s)", addr, tokenFile)
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second, // cap slow-header (Slowloris) clients
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second, // upstream Garmin calls can be slow
		IdleTimeout:       120 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}

// loadEnvFile reads simple KEY=VALUE lines from path into the environment
// (existing env vars win). Missing file is fine. Used for the gitignored secrets.env.
func loadEnvFile(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if k != "" && os.Getenv(k) == "" {
			_ = os.Setenv(k, v)
		}
	}
}

func (s *server) render(w http.ResponseWriter, name string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	data["LoggedIn"] = s.auth.LoggedIn()
	data["AutoLogin"] = s.auth.AutoLogin()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("template %s: %v", name, err)
		http.Error(w, err.Error(), 500)
	}
}

func (s *server) getDevices() ([]garmin.Device, error) {
	s.devMu.Lock()
	defer s.devMu.Unlock()
	if len(s.devices) > 0 && time.Since(s.devAt) < time.Hour {
		return s.devices, nil
	}
	d, err := s.client.DeviceTypes()
	if err != nil {
		return nil, err
	}
	// De-duplicate by name and sort alphabetically for a browsable list.
	sort.Slice(d, func(i, j int) bool { return strings.ToLower(d[i].Name) < strings.ToLower(d[j].Name) })
	s.devices, s.devAt = d, time.Now()
	return d, nil
}

func (s *server) device(id string) (garmin.Device, bool) {
	ds, err := s.getDevices()
	if err != nil {
		return garmin.Device{}, false
	}
	for _, d := range ds {
		if d.ID == id {
			return d, true
		}
	}
	return garmin.Device{}, false
}

// ensureAuth guarantees a session before a handler proceeds. Returns true if
// authed. Otherwise it writes the right response — in headless (auto-login) mode
// it triggers/awaits login and shows an error on failure; interactively it shows
// the login form (form=true) or redirects home (form=false) — and returns false.
func (s *server) ensureAuth(w http.ResponseWriter, r *http.Request, form bool) bool {
	if s.auth.LoggedIn() {
		return true
	}
	if s.auth.AutoLogin() {
		if err := s.auth.EnsureLogin(); err != nil {
			s.render(w, "error.html", map[string]any{"Err": "Auto-login failed: " + err.Error()})
			return false
		}
		return true
	}
	if form {
		s.render(w, "login.html", map[string]any{})
	} else {
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
	return false
}

func (s *server) handleHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if !s.ensureAuth(w, r, true) {
		return
	}
	devices, err := s.getDevices()
	if err != nil {
		s.render(w, "error.html", map[string]any{"Err": err.Error()})
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q != "" {
		lq := strings.ToLower(q)
		var f []garmin.Device
		for _, d := range devices {
			if strings.Contains(strings.ToLower(d.Name), lq) || strings.Contains(strings.ToLower(d.PartNumber), lq) {
				f = append(f, d)
			}
		}
		devices = f
	}
	s.render(w, "devices.html", map[string]any{"Devices": devices, "Q": q, "Count": len(devices)})
}

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if s.auth.AutoLogin() { // headless mode owns the session
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	email := r.FormValue("email")
	password := r.FormValue("password")
	if err := s.auth.Login(email, password); err != nil {
		msg := err.Error()
		if errors.Is(err, garmin.ErrMFA) {
			msg = "This account has 2FA enabled, which isn't supported."
		}
		s.render(w, "login.html", map[string]any{"Err": msg})
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.auth.Logout()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *server) handleDevice(w http.ResponseWriter, r *http.Request) {
	if !s.ensureAuth(w, r, false) {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/device/")
	dev, ok := s.device(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	qp := r.URL.Query()
	appType := qp.Get("type")
	if appType == "" {
		appType = "watchface"
	}
	sortType := qp.Get("sort")
	search := strings.TrimSpace(qp.Get("q"))
	page, _ := strconv.Atoi(qp.Get("page"))
	if page < 0 {
		page = 0
	}

	q := garmin.AppQuery{
		PartNumber: dev.PartNumber,
		AppType:    appType, SortType: sortType, Page: page, PageSize: 30,
	}

	var apps []garmin.App
	var err error
	switch {
	case search != "":
		apps, err = s.client.Search(q, search, "en-US")
	case appType == "FEATURED":
		apps, err = s.client.Featured(q)
	default:
		apps, err = s.client.Apps(q)
	}
	if err != nil {
		s.render(w, "error.html", map[string]any{"Err": err.Error()})
		return
	}

	s.render(w, "device.html", map[string]any{
		"Device":   dev,
		"Apps":     apps,
		"AppTypes": garmin.AppTypes,
		"Sorts":    garmin.SortTypes,
		"Type":     appType,
		"Sort":     sortType,
		"Q":        search,
		"Page":     page,
		"NextPage": page + 1,
		"PrevPage": page - 1,
		"HasMore":  len(apps) >= 30,
		"Count":    len(apps),
	})
}

func (s *server) handleApp(w http.ResponseWriter, r *http.Request) {
	if !s.ensureAuth(w, r, false) {
		return
	}
	appID := strings.TrimPrefix(r.URL.Path, "/app/")
	unit := r.URL.Query().Get("unit") // device id, used to scope + link back
	var dev garmin.Device
	if unit != "" {
		dev, _ = s.device(unit)
	}
	app, err := s.client.AppDetail(appID, dev.PartNumber, "US")
	if err != nil {
		s.render(w, "error.html", map[string]any{"Err": err.Error()})
		return
	}
	s.render(w, "app.html", map[string]any{"App": app, "Device": dev, "Unit": unit})
}
