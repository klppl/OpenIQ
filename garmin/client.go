package garmin

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	apiBase  = "https://services.garmin.com/appstore/api"
	clientID = "APPSTORE_MOBILE"
)

// ImageBase is the public (no-auth) CDN root for icons/screenshots/heroimages.
const ImageBase = apiBase

type Client struct {
	auth *Auth
	http *http.Client
}

func NewClient(auth *Auth) *Client {
	return &Client{auth: auth, http: &http.Client{Timeout: 30 * time.Second}}
}

// ---- response models (JSON keys match the app's DTOs) ----

type Device struct {
	ID              string   `json:"id"`
	PartNumber      string   `json:"partNumber"`
	Name            string   `json:"name"`
	AdditionalNames []string `json:"additionalNames"`
}

type Localization struct {
	Locale          string `json:"locale"`
	Name            string `json:"name"`
	Description     string `json:"description"`
	WhatsNew        string `json:"whatsNew"`
	HeroImageFileID string `json:"heroImageFileId"`
}

type Price struct {
	Amount       json.Number `json:"amount"`
	CurrencyCode string      `json:"currencyCode"`
}
type Pricing struct {
	ListPrice *Price `json:"listPrice"`
	SalePrice *Price `json:"salePrice"`
}
type Developer struct {
	Name string `json:"name"`
	ID   string `json:"id"`
}

type App struct {
	ID                          string         `json:"id"`
	AppLocalizations            []Localization `json:"appLocalizations"`
	TypeID                      string         `json:"typeId"`
	IconFileID                  string         `json:"iconFileId"`
	ScreenshotFileIDs           []string       `json:"screenshotFileIds"`
	AverageRating               *float64       `json:"averageRating"`
	ReviewCount                 *int           `json:"reviewCount"`
	DownloadCount               *int           `json:"downloadCount"`
	DeveloperID                 string         `json:"developerId"`
	Developer                   *Developer     `json:"developer"`
	CompatibleDevicePartNumbers []string       `json:"compatibleDevicePartNumbers"`
	Permissions                 []string       `json:"permissions"`
	HasTrialMode                bool           `json:"hasTrialMode"`
	IsTrending                  bool           `json:"isTrending"`
	Pricing                     *Pricing       `json:"pricing"`
	LatestExternalVersion       string         `json:"latestExternalVersion"`
	ChangedDate                 json.Number    `json:"changedDate"`
	WebsiteURL                  string         `json:"websiteUrl"`
	VideoURL                    string         `json:"videoUrl"`
	PrivacyPolicyURL            string         `json:"privacyPolicyUrl"`
	SupportEmailAddress         string         `json:"supportEmailAddress"`
}

func (a App) Name() string        { return a.loc().Name }
func (a App) Description() string  { return a.loc().Description }
func (a App) WhatsNew() string     { return a.loc().WhatsNew }
func (a App) HeroImageID() string  { return a.loc().HeroImageFileID }

func (a App) loc() Localization {
	for _, l := range a.AppLocalizations {
		if l.Locale == "en-US" || l.Locale == "en" {
			return l
		}
	}
	if len(a.AppLocalizations) > 0 {
		return a.AppLocalizations[0]
	}
	return Localization{}
}

func (a App) DeveloperName() string {
	if a.Developer != nil {
		return a.Developer.Name
	}
	return ""
}

func (a App) PriceLabel() string {
	if a.Pricing == nil || a.Pricing.ListPrice == nil {
		return "Free"
	}
	amt := a.Pricing.ListPrice.Amount.String()
	if amt == "" || amt == "0" || amt == "0.00" {
		return "Free"
	}
	p := a.Pricing.ListPrice
	if a.Pricing.SalePrice != nil && a.Pricing.SalePrice.Amount.String() != "" {
		p = a.Pricing.SalePrice
	}
	return fmt.Sprintf("%s %s", p.Amount.String(), p.CurrencyCode)
}

// Changed formats the last-updated timestamp (ms since epoch) as a date.
func (a App) Changed() string {
	if a.ChangedDate == "" {
		return ""
	}
	ms, err := a.ChangedDate.Int64()
	if err != nil {
		return ""
	}
	return time.Unix(ms/1000, 0).UTC().Format("Jan 2, 2006")
}

// AppType metadata (values are the store's exact app-type slugs).
type AppType struct{ Key, Label string }

var AppTypes = []AppType{
	{"watchface", "Watch Faces"},
	{"watch-app", "Apps"},
	{"widget", "Widgets"},
	{"datafield", "Data Fields"},
	{"audio-content-provider-app", "Music"},
}

// SortTypes: the real values are camelCase (the store's error message misleadingly
// lists UPPER_SNAKE). "mostRelevant" is the default and 400s if sent explicitly, so
// it maps to the empty (unset) value.
var SortTypes = []struct{ Key, Label string }{
	{"", "Most Relevant"},
	{"highestRated", "Highest Rated"},
	{"mostPopular", "Most Popular"},
	{"mostRecent", "Most Recent"},
	{"trending", "Trending"},
}

// ---- HTTP core ----

// encodeParams renders params as a sorted, RFC3986-encoded query string so that
// what we send exactly matches what we sign.
func encodeParams(v url.Values) string {
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		for _, val := range v[k] {
			parts = append(parts, pctEncode(k)+"="+pctEncode(val))
		}
	}
	return strings.Join(parts, "&")
}

// signedGet performs an OAuth1-signed store request and unmarshals the result.
func (c *Client) signedGet(path string, params url.Values, partNumber string, out any) error {
	base := apiBase + path
	auth, err := c.auth.SignStore("GET", base, params)
	if err != nil {
		return err
	}
	req, _ := http.NewRequest("GET", base+"?"+encodeParams(params), nil)
	req.Header.Set("Authorization", auth)
	req.Header.Set("X-garmin-client-id", clientID)
	req.Header.Set("User-Agent", uaOAuth)
	req.Header.Set("Accept", "application/json")
	if partNumber != "" {
		req.Header.Set("X-Garmin-SW-Part-Number", partNumber)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("store %s -> HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

// DeviceTypes returns the full model list (public endpoint, no login needed).
func (c *Client) DeviceTypes() ([]Device, error) {
	req, _ := http.NewRequest("GET", apiBase+"/deviceTypes", nil)
	req.Header.Set("X-garmin-client-id", clientID)
	req.Header.Set("User-Agent", uaOAuth)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("deviceTypes HTTP %d", resp.StatusCode)
	}
	var d []Device
	return d, json.Unmarshal(body, &d)
}

type AppQuery struct {
	PartNumber string // device to scope results to (X-Garmin-SW-Part-Number)
	AppType    string
	SortType   string
	Page       int
	PageSize   int
	Country    string
}

func (q AppQuery) values() url.Values {
	if q.PageSize == 0 {
		q.PageSize = 30
	}
	if q.Country == "" {
		q.Country = "US"
	}
	v := url.Values{
		"pageSize": {strconv.Itoa(q.PageSize)},
		// The store treats startPageIndex as a record offset, not a page number,
		// so advance by a full page each step (page 1 -> record 30, not 1).
		"startPageIndex":       {strconv.Itoa(q.Page * q.PageSize)},
		"countryCode":          {q.Country},
		"companionAppPlatform": {"ANDROID"},
	}
	if q.SortType != "" {
		v.Set("sortType", q.SortType)
	}
	if q.AppType != "" {
		v.Set("appType", q.AppType)
	}
	return v
}

// Apps lists apps compatible with a device model, filtered/sorted.
func (c *Client) Apps(q AppQuery) ([]App, error) {
	var apps []App
	err := c.signedGet("/asm/apps", q.values(), q.PartNumber, &apps)
	return apps, err
}

// Featured lists the curated featured apps for a device model.
func (c *Client) Featured(q AppQuery) ([]App, error) {
	v := q.values()
	v.Del("sortType")
	v.Del("appType")
	var apps []App
	err := c.signedGet("/asm/apps/featured", v, q.PartNumber, &apps)
	return apps, err
}

// Search runs a keyword search scoped to a device model.
func (c *Client) Search(q AppQuery, keywords, locale string) ([]App, error) {
	v := q.values()
	if locale == "" {
		locale = "en-US"
	}
	v.Set("keywords", keywords)
	v.Set("locale", locale)
	var apps []App
	err := c.signedGet("/asm/apps/keywords", v, q.PartNumber, &apps)
	return apps, err
}

// AppDetail fetches a single app by id.
func (c *Client) AppDetail(appID, partNumber, country string) (*App, error) {
	if country == "" {
		country = "US"
	}
	v := url.Values{"countryCode": {country}, "ignoreCompatibility": {"true"}}
	var app App
	if err := c.signedGet("/asm/apps/"+appID, v, partNumber, &app); err != nil {
		return nil, err
	}
	return &app, nil
}
