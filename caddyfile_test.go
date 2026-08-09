package caddygeoip

import (
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"

	// Registers the standard HTTP directives the adapter needs to turn a full
	// Caddyfile into JSON.
	_ "github.com/caddyserver/caddy/v2/modules/standard"
)

func parse(t *testing.T, input string) (*GeoIP, error) {
	t.Helper()
	var g GeoIP
	err := g.UnmarshalCaddyfile(caddyfile.NewTestDispenser(input))
	return &g, err
}

func TestUnmarshalCaddyfileFullBlock(t *testing.T) {
	g, err := parse(t, `traq_geoip {
		db_path /var/lib/traq/GeoLite2-City.mmdb
		languages de en
		reload_frequency 15m
		download_url https://example.test/GeoLite2-City.tar.gz
		download_frequency 12h
		account_id 123456
		license_key secret
	}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if g.DBPath != "/var/lib/traq/GeoLite2-City.mmdb" {
		t.Errorf("db_path = %q", g.DBPath)
	}
	if len(g.Languages) != 2 || g.Languages[0] != "de" || g.Languages[1] != "en" {
		t.Errorf("languages = %v", g.Languages)
	}
	if time.Duration(g.ReloadFrequency) != 15*time.Minute {
		t.Errorf("reload_frequency = %v", time.Duration(g.ReloadFrequency))
	}
	if time.Duration(g.DownloadFrequency) != 12*time.Hour {
		t.Errorf("download_frequency = %v", time.Duration(g.DownloadFrequency))
	}
	if g.DownloadURL != "https://example.test/GeoLite2-City.tar.gz" {
		t.Errorf("download_url = %q", g.DownloadURL)
	}
	if g.AccountID != "123456" || g.LicenseKey != "secret" {
		t.Errorf("credentials = %q / %q", g.AccountID, g.LicenseKey)
	}
}

// A bare argument would be consumed as a request matcher by the Caddyfile
// adapter — `traq_geoip /data/GeoLite2-City.mmdb` adapts to a path matcher on
// that literal path with an empty db_path. Rejecting it at parse time is the
// only way that mistake surfaces before runtime.
func TestUnmarshalCaddyfileRejectsBareArgument(t *testing.T) {
	_, err := parse(t, `traq_geoip /data/GeoLite2-City.mmdb`)
	if err == nil {
		t.Fatal("expected an error for a bare path argument")
	}
	if !strings.Contains(err.Error(), "db_path") {
		t.Errorf("error should point at db_path, got: %v", err)
	}
}

func TestUnmarshalCaddyfileErrors(t *testing.T) {
	cases := map[string]string{
		"bare argument":   `traq_geoip /a.mmdb`,
		"unknown option":  "traq_geoip {\n\ttrust_header X-Forwarded-For\n}",
		"bad duration":    "traq_geoip {\n\treload_frequency nope\n}",
		"languages empty": "traq_geoip {\n\tlanguages\n}",
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parse(t, input); err == nil {
				t.Fatal("expected an error, got none")
			}
		})
	}
}

// Parsing the directive in isolation is not enough: the bug this guards
// against only appears once the full Caddyfile adapter runs, because that is
// where the matcher token is extracted. Adapting the real thing is the only
// check that sees it.
func TestAdaptFullCaddyfile(t *testing.T) {
	adapter := caddyconfig.GetAdapter("caddyfile")
	if adapter == nil {
		t.Fatal("caddyfile adapter is not registered")
	}

	t.Run("block form carries db_path into the JSON config", func(t *testing.T) {
		result, _, err := adapter.Adapt([]byte(`:8080 {
	traq_geoip {
		db_path /data/GeoLite2-City.mmdb
	}
	respond "ok"
}`), nil)
		if err != nil {
			t.Fatalf("adapt: %v", err)
		}

		json := string(result)
		if !strings.Contains(json, `"handler":"traq_geoip"`) {
			t.Errorf("handler missing from config: %s", json)
		}
		if !strings.Contains(json, `"db_path":"/data/GeoLite2-City.mmdb"`) {
			t.Errorf("db_path missing from config: %s", json)
		}
		if strings.Contains(json, `"path":["/data/GeoLite2-City.mmdb"]`) {
			t.Errorf("db_path was turned into a path matcher: %s", json)
		}
	})

	// The adapter extracts a leading `/…` token as a request matcher before the
	// module's own parser runs, so this cannot be caught at parse time. It
	// adapts cleanly into a config that would never resolve anything — the
	// failure surfaces in Validate, and its message has to name the cause.
	t.Run("bare path is swallowed as a matcher, leaving db_path empty", func(t *testing.T) {
		result, _, err := adapter.Adapt([]byte(`:8080 {
	traq_geoip /data/GeoLite2-City.mmdb
}`), nil)
		if err != nil {
			t.Fatalf("adapt: %v", err)
		}

		json := string(result)
		if !strings.Contains(json, `"path":["/data/GeoLite2-City.mmdb"]`) {
			t.Errorf("expected the path to end up as a matcher: %s", json)
		}
		if strings.Contains(json, `"db_path"`) {
			t.Errorf("db_path should be absent: %s", json)
		}

		err = (&GeoIP{}).Validate()
		if err == nil {
			t.Fatal("expected Validate to reject an empty db_path")
		}
		if !strings.Contains(err.Error(), "matcher") {
			t.Errorf("error should name the matcher pitfall, got: %v", err)
		}
	})
}

// Defaults are applied in Provision, but Validate must reject the combinations
// that would silently degrade geo resolution before we ever get there.
func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		geoip   GeoIP
		wantErr bool
	}{
		{"path only", GeoIP{DBPath: "/a.mmdb"}, false},
		{"no path", GeoIP{}, true},
		{"negative reload", GeoIP{DBPath: "/a.mmdb", ReloadFrequency: caddy.Duration(-time.Second)}, true},
		{"download frequency without url", GeoIP{DBPath: "/a.mmdb", DownloadFrequency: caddy.Duration(time.Hour)}, true},
		{"download frequency with url", GeoIP{DBPath: "/a.mmdb", DownloadURL: "https://example.test/db", DownloadFrequency: caddy.Duration(time.Hour)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.geoip.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// A miss must produce every placeholder as an empty string — a partially
// populated set would let stale values from a previous request leak through
// the replacer.
func TestEmptyValuesCoversAllFields(t *testing.T) {
	values := emptyValues()
	if len(values) != len(Fields) {
		t.Fatalf("emptyValues() has %d entries, Fields has %d", len(values), len(Fields))
	}
	for _, f := range Fields {
		if v, ok := values[f]; !ok || v != "" {
			t.Errorf("field %q: value %q, present %v", f, v, ok)
		}
	}
}

func TestFormatCoord(t *testing.T) {
	cases := map[float64]string{
		47.3667:  "47.3667",
		-122.331: "-122.331",
		0:        "0",
		8.55:     "8.55",
	}
	for in, want := range cases {
		if got := formatCoord(in); got != want {
			t.Errorf("formatCoord(%v) = %q, want %q", in, got, want)
		}
	}
}
