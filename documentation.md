# How OpenIQ was built — reverse-engineering the Connect IQ store

This is the engineering story behind OpenIQ: how the Connect IQ store's mobile API
and its (surprisingly deep) authentication were reverse-engineered, the dead ends
that mattered, and how the final tool is put together.

> **Legal / ethics note.** This is an unofficial tool that talks to Garmin's
> private mobile API using client credentials extracted from the Android app. It's
> for personal use. It is against Garmin's ToS to redistribute their credentials or
> run a public service on them. Nothing here is endorsed by Garmin. See the caveats
> at the end.

---

## 0. The problem

Garmin retired the web version of the Connect IQ store — you can no longer browse
new watch faces / apps from a browser, only from the Connect IQ **phone app**. Goal:
rebuild a browser-based "pick a device model → see its compatible apps" experience by
talking to whatever API the phone app uses.

Starting point: the base APK (`com.garmin.connectiq`, ~69 MB), extracted in a folder.

---

## 1. Tooling

- **jadx** — decompile the 8 `.dex` files to readable-ish Java (`jadx -d jadx-out --no-res app.apk`).
- **`strings` + grep** — fast first pass over the raw `.dex` for URLs, endpoints, header names.
- **curl / python** — probe the live API and iterate on auth, headers, and params.
- No emulator or device was needed — everything was done statically + against the live servers.

Kotlin + R8 obfuscation renames everything to short names (`rn`, `wj6`, `s06`, …), but
**string literals survive** — which is what made this tractable.

---

## 2. Finding the API surface

`strings` over the dex immediately surfaced the host and endpoint shapes:

```
https://services.garmin.com/appstore/api/...
appstore/api/deviceTypes
appstore/api/asm/apps
appstore/api/asm/apps/{appId}
appstore/api/asm/apps/featured
appstore/api/asm/apps/keywords          # search
appstore/api/asm/calculatedCategories   # categories
appstore/api/icons/  /screenshots/  /heroimages/
```

A first probe returned a useful error:

```
$ curl https://services.garmin.com/appstore/api/deviceTypes
{"className":"AuthorizationException","message":"X-garmin-client-id-header not found."}
```

Guessing the client id (`APPSTORE_MOBILE`) unlocked the public endpoints:

```
$ curl -H 'X-garmin-client-id: APPSTORE_MOBILE' .../appstore/api/deviceTypes
[{"id":"20","partNumber":"006-B2431-00","name":"Forerunner® 235", ...}, ...]   # 354 models
```

### Decoding the Retrofit interfaces

The endpoint contract lives in Retrofit interfaces, but the annotations are obfuscated.
Cross-referencing the decompiled interfaces gave the mapping:

| Obfuscated | Retrofit |
|---|---|
| `@pn2` | `@GET` |
| `@fz4` | `@POST` |
| `@o91` | `@DELETE` |
| `@w15` | `@Path` |
| `@kg5` | `@Query` |
| `@wy2` | `@Header` |
| `@r40` | `@Body` |

So e.g. the browse method decodes to:

```
@GET("appstore/api/asm/apps")
apps(@Query sortType, @Query appType, @Query pageSize, @Query startPageIndex,
     @Query countryCode, @Query unitId, @Header("X-Garmin-SW-Part-Number") partNumber,
     @Query companionAppPlatform)
```

**Public (no auth):** `deviceTypes`, `permissions`, and the image CDN.
**Everything else → 401.** That kicked off the real work.

---

## 3. The authentication rabbit hole

This was 90% of the effort. Here is the sequence of hypotheses and what each returned.

### 3a. Standard Garmin SSO → OAuth2 bearer (the obvious path)

The app uses the well-known Garmin SSO flow (the same one the `garth` library
implements): log in at `sso.garmin.com`, get a ticket, exchange it for an OAuth1 user
token, then exchange that for an OAuth2 "DI" bearer:

```
sso.garmin.com/sso/embed                                   # prime cookies
sso.garmin.com/sso/signin                                  # scrape _csrf, POST creds → ticket
connectapi.garmin.com/oauth-service/oauth/preauthorized    # ticket → OAuth1 token
connectapi.garmin.com/oauth-service/oauth/exchange/user/2.0 # OAuth1 → OAuth2 DI bearer
```

The bearer even carried an appstore scope (`CIQ_APPSTORE_SERVICES_READ`). But:

```
GET /asm/apps  (Authorization: Bearer <DI bearer>)
→ 401 {"error":"invalid_token","error_description":
       "Invalid issuer \"https://diauth.garmin.com\" specified in JWT access token!"}
```

**Dead end #1.** The appstore refuses the standard `diauth` bearer.

### 3b. The "IT" OAuth2 token

The decompiled auth layer has *two* OAuth2 schemes: **DI** and **IT**. Following the
`IT` path led to a different token endpoint:

```
POST services.garmin.com/api/oauth/token?grant_type=connect_exchange
     body: client_id=CIQ_APPSTORE_MOBILE&connect_access_token=<user OAuth1 token>
→ 200 { "access_token": "...", "token_type": "Bearer", ... }
```

This IT token **passed appstore auth** — no more "invalid issuer". Progress! But every
user-scoped call then failed deeper in:

```
GET /asm/apps  (Authorization: Bearer <IT token>)
→ 500 NullPointerException: Cannot invoke
      "com.garmin.di.appstore.sec.model.ConnectUserCredentials.getCustomerId()"
      because "credentials" is null
```

**Dead end #2.** The token authenticates, but the server can't resolve *who* you are.
A long detour followed — trying `connect_exchange` vs `connect2_exchange`, the
`service_ticket` grant, re-issuing tokens under different consumers — all still
`ConnectUserCredentials is null`.

### 3c. The breakthrough: the store uses OAuth1 *request signing*, not a bearer

The DI graph binds each Retrofit interface to a named HTTP client. The browse
interfaces (`wj6`, `s06`, `rn`, …) resolve to the qualifier:

```
RETROFIT_CIQ_OAUTH_1
```

Not `RETROFIT_CIQ_OAUTH2_IT`. **The store signs each browse request with OAuth1
(HMAC-SHA1), and derives the user identity from the signature** — a bearer token was
never the right mechanism. Testing this directly finally returned data (a `400` about
a bad param value, which is auth *succeeding*):

```
GET /asm/apps?...  (Authorization: OAuth oauth_consumer_key=..., oauth_signature=...)
→ 400 "Invalid app-type! Only these are valid: ... watchface, watch-app, widget ..."
```

### 3d. The consumer secret is in a native library

OAuth1 signing needs the app's **consumer key + secret**. The decompiled code fetches
them through `com.garmin.util.StringRetriever`, which is a **JNI call into `libsr.so`** —
a native library, deliberately used to hide the credentials. Critically, `libsr.so`
lives in the `config.arm64_v8a` **split APK**, not the base APK — so the full app
bundle (`.apkm`/`.xapk`) was needed.

Once extracted, the credentials turned out to be **plaintext** inside the `.so`:

```
$ strings libsr.so | grep -A1 CIQ_APPSTORE_MOBILE
04d64e0b-...  ,  <secret>  , ... , CIQ_APPSTORE_MOBILE_ANDROID_DI , CIQ_APPSTORE_MOBILE
```

This is the private **`ConnectIQMobileAndroid`** OAuth1 consumer (distinct from the
public Garmin Connect Mobile consumer used only for login). Redacted here — it lives in
`secrets.env` (gitignored). Extract your own with the command above.

### 3e. Re-issue the user token under the CIQ consumer

The user's OAuth1 token from login is scoped to the *public* login consumer. The store
needs it under the *CIQ* consumer. Garmin has an endpoint exactly for that:

```
GET connectapi.garmin.com/oauth-service/oauth/tokens/consumer
    (OAuth1-signed with the CIQ consumer, presenting the user's login token)
→ { "token": "...", "secret": "..." }   # same user, now CIQ-consumer-scoped
```

No second login required — the login token is simply re-minted for the target consumer.

### The final working auth chain

```
1. SSO login (public consumer)         → OAuth1 user token
2. tokens/consumer (CIQ consumer)      → CIQ-scoped user token + secret
3. OAuth1-sign every /appstore/api/... request with the CIQ consumer + that token
```

That's it. No bearer at all for browsing. The whole IT/DI bearer investigation was a
red herring the store's own error messages actively encouraged.

---

## 4. Parameter gotchas (all discovered empirically)

The store's 400 error messages are frequently **misleading** — they list values that
don't actually work. What's real:

- **Browse any model by part number.** `/asm/apps` with `unitId` requires a device the
  account *owns* (a real unit serial — the `deviceTypes` `id` is rejected as "Invalid
  unitId"). To browse an arbitrary model, **omit `unitId`** and pass the model's
  `partNumber` in the `X-Garmin-SW-Part-Number` header. Verified this returns
  device-appropriate, compatible results per model.
- **`appType`** is lowercase-hyphen: `watchface, watch-app, widget, datafield,
  audio-content-provider-app, background` — *not* the `WATCH_FACE` the code constants
  suggest.
- **`sortType`** is **camelCase**: `highestRated, mostPopular, mostRecent, trending,
  hotFresh`. The error message lists `MOST_RECENT`, `HIGHEST_RATED`, … in UPPER_SNAKE —
  all of which 400. `mostRelevant` is the default and 400s if sent explicitly (send the
  empty value instead). The real values were found in the app's own web `<select>`.
- **`featured`** is curated per-device and frequently empty.
- **`changedDate`** is a numeric ms-epoch timestamp, not a string.
- Images (`/icons`, `/screenshots`, `/heroimages`) are **public** — embed directly.

---

## 5. What connects to whom (identity model)

- **Consumer key/secret** (in `secrets.env`) = **Garmin's**, identical in every Connect
  IQ install. Not tied to any person. Identifies "the app."
- **User token** (in the token cache, e.g. `~/.config/…` or the Docker `/data` volume) =
  **you** — created at login, tied to your Garmin account.
- Email/password are sent only to `sso.garmin.com` at login and never written to disk.
- Each request is signed with **both** → Garmin sees "this account, via the CIQ app."

---

## 6. OpenIQ architecture

A single, dependency-free Go binary (standard library only; templates are `go:embed`ed).

```
main.go            HTTP server, routes, env loading, headless auto-login
garmin/auth.go     SSO login + OAuth1 HMAC-SHA1 signing + token cache
garmin/client.go   appstore API client + response models
templates/*.html   UI (device picker, browse-by-type, search, app detail)
```

Key implementation points:
- **OAuth1 signing** (`oauth1Sign`) is hand-rolled: RFC-3986 percent-encoding, sorted
  param base string, HMAC-SHA1, `Authorization: OAuth ...` header. The query string sent
  is encoded identically to the signature base so they always match.
- **Auth is lazy + cached.** Login yields the OAuth1 token; the CIQ-scoped token is
  re-minted on demand and cached to disk (`tokens.json`). OAuth1 user tokens are
  long-lived, so there's no short-TTL refresh dance.
- **Secrets come from the environment** (`GARMIN_CIQ_CONSUMER_KEY/SECRET`), loaded from a
  gitignored `secrets.env` — never hardcoded, never committed.
- **Headless mode**: set `GARMIN_EMAIL/PASSWORD` and the app logs in on startup and
  hides the login form (for a server deployment behind e.g. Cloudflare Zero Trust).
- **Delivery**: multi-stage `Dockerfile` (static binary on Alpine, non-root, token
  volume), `docker-compose.yml`, and a manual GitHub Actions workflow that publishes to
  GHCR.

### The request, end to end

```
Browser → OpenIQ (Go)
  ├─ ensure session: SSO login (once) → OAuth1 token → CIQ-scoped token (cached)
  └─ GET services.garmin.com/appstore/api/asm/apps?appType=watchface&...&companionAppPlatform=ANDROID
       Headers: X-garmin-client-id: APPSTORE_MOBILE
                X-Garmin-SW-Part-Number: <model partNumber>
                Authorization: OAuth <signed with CIQ consumer + user token>
     → JSON list of apps → rendered as cards
```

---

## 7. Lessons

- **Obfuscation hides names, not strings.** `strings` on the dex + a live probe got 80%
  of the API surface before opening a decompiler.
- **Trust the wiring, not the error messages.** The server's `400`/`500` texts pointed
  at the wrong token type and listed non-working enum values. The DI-graph qualifier
  (`RETROFIT_CIQ_OAUTH_1`) was the single fact that cracked auth.
- **"Valid issuer" ≠ "authorized".** A token can pass auth and still fail because the
  server resolves identity a different way (here, from the OAuth1 signature, not the JWT).
- **Native libs are the last mile.** The one secret that couldn't be found in the dex was
  in `libsr.so` — and it was plaintext once you had the right APK split.

---

## 8. Caveats

- Unofficial and against Garmin's ToS; use a **dedicated account** for any server
  deployment, and never commit the consumer secret.
- Garmin can rotate the consumer key or change the API at any time and break this.
- MFA/2FA accounts are not supported by the login flow.
