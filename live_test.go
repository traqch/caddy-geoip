package caddygeoip

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The GeoLite2 free tier allows only a handful of downloads per day, so this
// test never runs on its own. Opt in explicitly:
//
//	TRAQ_MAXMIND_LIVE=1 go test -run TestLiveMaxMindDownload -v
//
// The key is read from TRAQ_MAXMIND_KEY_FILE, defaulting to
// ~/.traq/maxmind-key. It is never logged.
const (
	liveEnvVar     = "TRAQ_MAXMIND_LIVE"
	keyFileEnvVar  = "TRAQ_MAXMIND_KEY_FILE"
	defaultKeyFile = ".traq/maxmind-key"

	// The account-ID-less endpoint: with only a license key at hand, basic
	// auth against the newer /geoip/databases/ path is not possible.
	maxmindLegacyDownload = "https://download.maxmind.com/app/geoip_download"
)

// TestLiveMaxMindDownload proves the download path against the real thing —
// MaxMind's actual tar.gz layout, not the fixture that imitates it.
func TestLiveMaxMindDownload(t *testing.T) {
	if os.Getenv(liveEnvVar) != "1" {
		t.Skipf("set %s=1 to run this; it consumes the daily GeoLite2 download quota", liveEnvVar)
	}

	licenseKey := readLicenseKey(t)

	query := url.Values{
		"edition_id":  {"GeoLite2-City"},
		"license_key": {licenseKey},
		"suffix":      {"tar.gz"},
	}

	g := provision(t, &GeoIP{
		DBPath:      filepath.Join(t.TempDir(), "GeoLite2-City.mmdb"),
		DownloadURL: maxmindLegacyDownload + "?" + query.Encode(),
	})

	info, err := os.Stat(g.DBPath)
	if err != nil {
		t.Fatalf("database was not written: %v", err)
	}
	// The real City database is tens of megabytes; anything small means we
	// stored an error page that happened to survive verification.
	if info.Size() < 10<<20 {
		t.Errorf("database is only %d bytes", info.Size())
	}

	g.db.mu.RLock()
	dbType := g.db.reader.Metadata.DatabaseType
	buildEpoch := g.db.reader.Metadata.BuildEpoch
	g.db.mu.RUnlock()

	if dbType != "GeoLite2-City" {
		t.Errorf("database_type = %q", dbType)
	}
	t.Logf("live database: type=%s build_epoch=%d size=%d", dbType, buildEpoch, info.Size())

	// A Google resolver address: the exact city moves around, but the country
	// is stable enough to prove the record decodes.
	got := resolve(t, g, "8.8.8.8:443")
	if got[FieldCountryCode] != "US" {
		t.Errorf("8.8.8.8 resolved to country_code %q, want US", got[FieldCountryCode])
	}
	t.Logf("8.8.8.8 → %s / %s / %s (%s, %s)",
		got[FieldCountryCode], got[FieldRegionCode], got[FieldCity],
		got[FieldLatitude], got[FieldLongitude])
}

func readLicenseKey(t *testing.T) string {
	t.Helper()

	path := os.Getenv(keyFileEnvVar)
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatalf("resolve home directory: %v", err)
		}
		path = filepath.Join(home, defaultKeyFile)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read license key from %s: %v", path, err)
	}

	key := strings.TrimSpace(string(raw))
	if key == "" {
		t.Fatalf("license key file %s is empty", path)
	}
	return key
}
