// Package caddygeoip resolves the client IP to city-level geo data from a
// MaxMind GeoLite2-City database and exposes it as Caddy placeholders.
//
// It emits eight fields and no more, on the assumption that an edge forwards
// them to an upstream as request headers.
//
// The client IP is taken from the connection (RemoteAddr) only. There is
// deliberately no trust_header option: the real client address is expected to
// arrive over the PROXY protocol, and honouring a client-settable header would
// let any visitor pick their own geo — unacceptable once geo feeds consent
// decisions or cache keys.
package caddygeoip

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

// PlaceholderPrefix is prepended to every field name, so country_code is
// available as {traq.geoip.country_code}.
const PlaceholderPrefix = "traq.geoip."

const (
	defaultLanguage = "en"
	// Short enough that an out-of-band database update (init container, CSI
	// volume, sidecar) takes effect without a pod restart — the concrete
	// shortcoming of today's setup.
	defaultReloadFrequency = time.Hour
	// MaxMind publishes GeoLite2 twice a week; daily is the usual cadence and
	// stays well inside their rate limits.
	defaultDownloadFrequency = 24 * time.Hour
)

func init() {
	caddy.RegisterModule(GeoIP{})
	httpcaddyfile.RegisterHandlerDirective("traq_geoip", parseCaddyfile)
	// Must run before anything that reads the placeholders — header
	// manipulation and reverse_proxy both come later in the standard order.
	httpcaddyfile.RegisterDirectiveOrder("traq_geoip", httpcaddyfile.Before, "header")
}

// GeoIP is the HTTP handler that performs the lookup and sets the placeholders.
type GeoIP struct {
	// Path to the GeoLite2-City database. Required.
	DBPath string `json:"db_path,omitempty"`

	// Preferred languages for the localised names (country_name, region_name,
	// city), tried in order. Defaults to ["en"].
	Languages []string `json:"languages,omitempty"`

	// How often the file on disk is checked for changes and re-opened.
	// Defaults to 1h; set to 0 to disable.
	ReloadFrequency caddy.Duration `json:"reload_frequency,omitempty"`

	// Optional source to fetch the database from. Accepts a bare .mmdb, a
	// .gz, or MaxMind's .tar.gz.
	DownloadURL string `json:"download_url,omitempty"`

	// How often DownloadURL is re-fetched. Defaults to 24h when a URL is set.
	DownloadFrequency caddy.Duration `json:"download_frequency,omitempty"`

	// HTTP basic auth for the download. For MaxMind these are the account ID
	// and a license key.
	AccountID  string `json:"account_id,omitempty"`
	LicenseKey string `json:"license_key,omitempty"`

	db  *database
	log *zap.Logger
}

// CaddyModule returns the Caddy module information.
func (GeoIP) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.traq_geoip",
		New: func() caddy.Module { return new(GeoIP) },
	}
}

// Provision sets up the shared database handle.
func (g *GeoIP) Provision(ctx caddy.Context) error {
	g.log = ctx.Logger()

	repl := caddy.NewReplacer()
	g.DBPath = repl.ReplaceKnown(g.DBPath, "")
	g.DownloadURL = repl.ReplaceKnown(g.DownloadURL, "")
	g.AccountID = repl.ReplaceKnown(g.AccountID, "")
	g.LicenseKey = repl.ReplaceKnown(g.LicenseKey, "")

	if len(g.Languages) == 0 {
		g.Languages = []string{defaultLanguage}
	}
	if g.ReloadFrequency == 0 {
		g.ReloadFrequency = caddy.Duration(defaultReloadFrequency)
	}
	if g.DownloadURL != "" && g.DownloadFrequency == 0 {
		g.DownloadFrequency = caddy.Duration(defaultDownloadFrequency)
	}

	db, err := loadDatabase(dbConfig{
		Path:              g.DBPath,
		ReloadFrequency:   time.Duration(g.ReloadFrequency),
		DownloadURL:       g.DownloadURL,
		DownloadFrequency: time.Duration(g.DownloadFrequency),
		AccountID:         g.AccountID,
		LicenseKey:        g.LicenseKey,
	}, g.log)
	if err != nil {
		return err
	}
	g.db = db
	return nil
}

// Validate rejects configurations that would silently degrade geo resolution.
func (g *GeoIP) Validate() error {
	if g.DBPath == "" {
		// The most likely cause is `traq_geoip /path/to.mmdb`: the Caddyfile
		// adapter extracts that leading token as a request matcher before this
		// module ever sees it, so the config adapts cleanly and arrives here
		// with nothing set.
		return fmt.Errorf(
			"db_path is required and must be set inside the block; " +
				"a bare path after `traq_geoip` is parsed as a request matcher, not as the database path")
	}
	if g.ReloadFrequency < 0 || g.DownloadFrequency < 0 {
		return fmt.Errorf("frequencies must not be negative")
	}
	if g.DownloadFrequency > 0 && g.DownloadURL == "" {
		return fmt.Errorf("download_frequency is set but download_url is not")
	}
	return nil
}

// Cleanup releases this handler's reference to the shared database.
func (g *GeoIP) Cleanup() error {
	if g.db == nil {
		return nil
	}
	return releaseDatabase(g.DBPath)
}

// ServeHTTP sets all eight placeholders and passes the request on. A lookup
// miss is not an error: every placeholder is set to the empty string, which
// an upstream reads as "no geo" rather than as bogus geo.
func (g *GeoIP) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	repl, ok := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)
	if !ok {
		return fmt.Errorf("no replacer in request context")
	}

	values := emptyValues()
	if addr, ok := clientAddr(r); ok {
		if rec, found := g.db.lookup(addr); found {
			values = rec.values(g.Languages)
		}
	}

	for field, value := range values {
		repl.Set(PlaceholderPrefix+field, value)
	}

	return next.ServeHTTP(w, r)
}

// clientAddr reads the peer address off the connection. With the PROXY
// protocol listener wrapper in place this is the real visitor; without it, it
// is the load balancer, and every request resolves identically — the symptom
// worth checking first when geo looks uniform.
func clientAddr(r *http.Request) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	// Zones are meaningless for a lookup, and an IPv4-mapped IPv6 address
	// must resolve like the plain IPv4 one.
	return addr.WithZone("").Unmap(), true
}

// UnmarshalCaddyfile parses the traq_geoip directive:
//
//	traq_geoip [<matcher>] {
//	    db_path            <path>
//	    languages          <lang...>
//	    reload_frequency   <duration>
//	    download_url       <url>
//	    download_frequency <duration>
//	    account_id         <id>
//	    license_key        <key>
//	}
//
// There is deliberately no `traq_geoip <db_path>` shorthand. Every handler
// directive takes an optional matcher token as its first argument, and a
// leading `/…` is exactly a path matcher — so `traq_geoip /var/lib/db.mmdb`
// would silently become "run geoip on requests to /var/lib/db.mmdb" with an
// empty db_path. The only unambiguous place for the path is inside the block.
func (g *GeoIP) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // consume directive name

	if d.NextArg() {
		return d.Errf(
			"unexpected argument '%s': the database path belongs in the block as `db_path`, "+
				"a bare argument here would be parsed as a request matcher", d.Val())
	}

	for d.NextBlock(0) {
		switch d.Val() {
		case "db_path":
			if !d.AllArgs(&g.DBPath) {
				return d.ArgErr()
			}
		case "languages":
			g.Languages = d.RemainingArgs()
			if len(g.Languages) == 0 {
				return d.ArgErr()
			}
		case "reload_frequency":
			dur, err := parseDuration(d)
			if err != nil {
				return err
			}
			g.ReloadFrequency = dur
		case "download_url":
			if !d.AllArgs(&g.DownloadURL) {
				return d.ArgErr()
			}
		case "download_frequency":
			dur, err := parseDuration(d)
			if err != nil {
				return err
			}
			g.DownloadFrequency = dur
		case "account_id":
			if !d.AllArgs(&g.AccountID) {
				return d.ArgErr()
			}
		case "license_key":
			if !d.AllArgs(&g.LicenseKey) {
				return d.ArgErr()
			}
		default:
			return d.Errf("unrecognized traq_geoip option '%s'", d.Val())
		}
	}

	return nil
}

func parseDuration(d *caddyfile.Dispenser) (caddy.Duration, error) {
	var raw string
	if !d.AllArgs(&raw) {
		return 0, d.ArgErr()
	}
	dur, err := caddy.ParseDuration(raw)
	if err != nil {
		return 0, d.Errf("parsing duration '%s': %v", raw, err)
	}
	return caddy.Duration(dur), nil
}

func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var g GeoIP
	if err := g.UnmarshalCaddyfile(h.Dispenser); err != nil {
		return nil, err
	}
	return &g, nil
}

// Interface guards.
var (
	_ caddy.Provisioner           = (*GeoIP)(nil)
	_ caddy.Validator             = (*GeoIP)(nil)
	_ caddy.CleanerUpper          = (*GeoIP)(nil)
	_ caddyhttp.MiddlewareHandler = (*GeoIP)(nil)
	_ caddyfile.Unmarshaler       = (*GeoIP)(nil)
	_ caddy.Destructor            = (*database)(nil)
)
