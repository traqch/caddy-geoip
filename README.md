# caddy-geoip

A Caddy v2 plugin that resolves the client IP to city-level geo data from a MaxMind
GeoLite2-City database and exposes it as Caddy placeholders.

Module ID: `http.handlers.traq_geoip`, Caddyfile directive: `traq_geoip`.

Built for an edge that terminates TLS for many customer domains and forwards geo data to
an upstream as request headers, so its choices are deliberately narrow: eight fields, no
header-based IP trust, and a database that can be refreshed without restarting the process.

## Placeholders

| Placeholder | Source in the mmdb |
|---|---|
| `{traq.geoip.country_code}` | `country.iso_code` |
| `{traq.geoip.country_name}` | `country.names[lang]` |
| `{traq.geoip.region_code}` | `subdivisions[0].iso_code` |
| `{traq.geoip.region_name}` | `subdivisions[0].names[lang]` |
| `{traq.geoip.city}` | `city.names[lang]` |
| `{traq.geoip.postal_code}` | `postal.code` |
| `{traq.geoip.latitude}` | `location.latitude` |
| `{traq.geoip.longitude}` | `location.longitude` |

On a lookup miss — and for any field the record does not carry — **all eight** placeholders
are set to the empty string, never a partial set. Downstream code that treats an empty
header as absent therefore sees "no geo" rather than wrong geo.

Coordinates are decoded through pointers on purpose: a record without a `location` block
must not turn into `0/0`, which is a real position in the Gulf of Guinea.

## Client IP

Taken from `RemoteAddr` only. **There is no `trust_header` option, by design.**

If the process sits behind a load balancer, the real client address must arrive over the
PROXY protocol, not in a header. Honouring a client-settable header would make geo
spoofable by anyone, and geo that feeds consent decisions or cache keys is not something a
visitor should be able to choose.

The visible symptom of a missing PROXY protocol setup is that every request resolves
identically, to the balancer's own address.

## Caddyfile

```caddyfile
traq_geoip [<matcher>] {
    db_path            <path>
    languages          <lang...>
    reload_frequency   <duration>
    download_url       <url>
    download_frequency <duration>
    account_id         <id>
    license_key        <key>
}
```

| Option | Default | Meaning |
|---|---|---|
| `db_path` | – (required) | Path to the GeoLite2-City database |
| `languages` | `en` | Preferred languages for localised names, tried in order |
| `reload_frequency` | `1h` | How often the file is checked for changes and re-opened. `0` disables |
| `download_url` | – | Optional source. Accepts a bare `.mmdb`, a `.gz`, or MaxMind's `.tar.gz` |
| `download_frequency` | `24h` (when a URL is set) | How often the source is re-fetched |
| `account_id` / `license_key` | – | HTTP basic auth for the download |

```caddyfile
example.com {
    traq_geoip {
        db_path /var/lib/geoip/GeoLite2-City.mmdb
    }
    reverse_proxy upstream.internal:8080 {
        header_up X-Geo-Country {traq.geoip.country_code}
        header_up X-Geo-City    {traq.geoip.city}
    }
}
```

The directive is ordered before `header` via `RegisterDirectiveOrder`, so it runs before
anything that reads the placeholders. No `route` wrapper needed.

### There is no `traq_geoip <db_path>` shorthand

Every handler directive takes an optional matcher token as its first argument, and a
leading `/…` is exactly a path matcher. `traq_geoip /var/lib/db.mmdb` would silently mean
"run geoip on requests to /var/lib/db.mmdb" with an empty `db_path`. The adapter extracts
that token before the module sees it, so it cannot be rejected at parse time: the config
adapts cleanly and fails when loading, with an error that names this cause. The optional
matcher itself remains usable, e.g. `traq_geoip /api/*`.

## Refreshing without a restart

- **`reload_frequency`** compares the file's mtime and size, and only re-opens when they
  changed. The previous reader is closed after in-flight lookups finish, so an update from
  outside the process — init container, CSI volume, sidecar — takes effect on its own.
- **`download_frequency`** re-fetches, unpacks (gzip and tar are detected), **verifies the
  file before replacing anything** (it must open, and its `DatabaseType` must contain
  `City`) and then moves it into place with an atomic rename. If the download fails, the
  running database stays in service and an error is logged.

The database file must be replaced **atomically, via rename** — never overwritten in place.
The open reader memory-maps it; truncating underneath makes in-flight lookups fail with
`unexpected end of database` until the next reload. The built-in download does this
correctly; anything else that writes the file has to as well.

If the file is missing at startup and no `download_url` is configured, **Caddy will not
start**. That is intentional: silently serving without geo data is the more expensive
failure when downstream behaviour depends on it, and under an orchestrator a container that
refuses to start leaves the previous one running.

Several site blocks sharing a `db_path` share a single open database through a
`caddy.UsagePool`, so a config reload does not re-open a ~60 MB file.

## MaxMind sources

```caddyfile
# Account ID + license key (basic auth against the current endpoint)
download_url https://download.maxmind.com/geoip/databases/GeoLite2-City/download?suffix=tar.gz
account_id   {env.MAXMIND_ACCOUNT_ID}
license_key  {env.MAXMIND_LICENSE_KEY}

# License key only (classic endpoint, key as a query parameter)
download_url https://download.maxmind.com/app/geoip_download?edition_id=GeoLite2-City&suffix=tar.gz&license_key={env.MAXMIND_LICENSE_KEY}
```

The URL is redacted before it is logged: userinfo is dropped, and of the query parameters
only known-harmless ones survive (`edition_id`, `suffix`, `date`, `version`, `format`).
Everything else becomes `REDACTED` — an allowlist rather than a denylist, because an
unrecognised parameter is a credential until proven otherwise.

The GeoLite2 free tier permits only a few downloads per day. Keep `download_frequency` at
24h or above, and do not point several instances with different `db_path` values at the
same license.

## Build

```bash
xcaddy build --with github.com/traqch/caddy-geoip
```

## Development

```bash
go test -race ./...
```

Test databases are built at runtime with `mmdbwriter`; no `.mmdb` is committed, since
GeoLite2 data is licensed and a binary blob would not be reviewable.

A live test against MaxMind is skipped by default because it consumes the daily quota:

```bash
TRAQ_MAXMIND_LIVE=1 go test -run TestLiveMaxMindDownload -v
```

It reads the key from `TRAQ_MAXMIND_KEY_FILE`, defaulting to `~/.traq/maxmind-key`.

## License

See [LICENSE](LICENSE).
