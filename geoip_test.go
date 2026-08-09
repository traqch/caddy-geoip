package caddygeoip

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// provision builds a ready handler and registers its teardown. Every test gets
// its own db_path (t.TempDir), so the shared UsagePool never bleeds between
// tests.
func provision(t *testing.T, g *GeoIP) *GeoIP {
	t.Helper()

	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	t.Cleanup(cancel)

	if err := g.Provision(ctx); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	t.Cleanup(func() {
		if err := g.Cleanup(); err != nil {
			t.Errorf("Cleanup: %v", err)
		}
	})
	return g
}

// resolve runs one request through the handler and returns the placeholders it
// set. This is the real path — replacer included — not a direct lookup call.
func resolve(t *testing.T, g *GeoIP, remoteAddr string) map[string]string {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/collect", nil)
	req.RemoteAddr = remoteAddr

	repl := caddy.NewReplacer()
	ctx := context.WithValue(req.Context(), caddy.ReplacerCtxKey, repl)
	req = req.WithContext(ctx)

	var called bool
	next := caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error {
		called = true
		return nil
	})

	if err := g.ServeHTTP(httptest.NewRecorder(), req, next); err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	if !called {
		t.Fatal("next handler was not called")
	}

	out := make(map[string]string, len(Fields))
	for _, field := range Fields {
		val, ok := repl.GetString(PlaceholderPrefix + field)
		if !ok {
			t.Errorf("placeholder %s%s was never set", PlaceholderPrefix, field)
		}
		out[field] = val
	}
	return out
}

func assertFields(t *testing.T, got map[string]string, want map[string]string) {
	t.Helper()
	for _, field := range Fields {
		if got[field] != want[field] {
			t.Errorf("%s = %q, want %q", field, got[field], want[field])
		}
	}
}

// All eight fields, end to end: mmdb → lookup → placeholder.
func TestAllFieldsResolve(t *testing.T) {
	g := provision(t, &GeoIP{DBPath: writeTestDB(t, zurich(), countryOnly())})

	assertFields(t, resolve(t, g, "81.2.69.200:41234"), map[string]string{
		FieldCountryCode: "CH",
		FieldCountryName: "Switzerland",
		FieldRegionCode:  "ZH",
		FieldRegionName:  "Zurich",
		FieldCity:        "Zurich",
		FieldPostalCode:  "8001",
		FieldLatitude:    "47.3667",
		FieldLongitude:   "8.55",
	})
}

// The non-hit case: an address the database knows nothing about. Every
// placeholder must be present and empty, so nothing leaks and the upstream sees
// "no geo" rather than "wrong geo".
func TestLookupMiss(t *testing.T) {
	g := provision(t, &GeoIP{DBPath: writeTestDB(t, zurich())})

	assertFields(t, resolve(t, g, "198.51.100.7:1234"), map[string]string{
		FieldCountryCode: "", FieldCountryName: "", FieldRegionCode: "", FieldRegionName: "",
		FieldCity: "", FieldPostalCode: "", FieldLatitude: "", FieldLongitude: "",
	})
}

// A record that only carries a country must not invent the rest. The
// coordinates are the dangerous ones: 0/0 is a real position.
func TestSparseRecordLeavesFieldsEmpty(t *testing.T) {
	g := provision(t, &GeoIP{DBPath: writeTestDB(t, zurich(), countryOnly())})

	assertFields(t, resolve(t, g, "89.160.20.120:443"), map[string]string{
		FieldCountryCode: "SG",
		FieldCountryName: "Singapore",
		FieldRegionCode:  "", FieldRegionName: "", FieldCity: "", FieldPostalCode: "",
		FieldLatitude: "", FieldLongitude: "",
	})
}

func TestLanguagePreference(t *testing.T) {
	path := writeTestDB(t, zurich())

	t.Run("first language wins", func(t *testing.T) {
		g := provision(t, &GeoIP{DBPath: path, Languages: []string{"de", "en"}})
		got := resolve(t, g, "81.2.69.200:1")
		if got[FieldCity] != "Zürich" || got[FieldCountryName] != "Schweiz" {
			t.Errorf("city = %q, country_name = %q", got[FieldCity], got[FieldCountryName])
		}
	})

	t.Run("falls through to the next", func(t *testing.T) {
		g := provision(t, &GeoIP{DBPath: path, Languages: []string{"fr", "en"}})
		got := resolve(t, g, "81.2.69.200:1")
		if got[FieldCity] != "Zurich" {
			t.Errorf("city = %q, want the en fallback", got[FieldCity])
		}
	})

	t.Run("no match leaves the name empty", func(t *testing.T) {
		g := provision(t, &GeoIP{DBPath: path, Languages: []string{"fr"}})
		got := resolve(t, g, "81.2.69.200:1")
		if got[FieldCity] != "" || got[FieldCountryCode] != "CH" {
			t.Errorf("city = %q, country_code = %q", got[FieldCity], got[FieldCountryCode])
		}
	})
}

// IPv4-mapped IPv6 peers must resolve like the plain IPv4 address; a zone on a
// link-local address must not break parsing.
func TestClientAddrNormalisation(t *testing.T) {
	g := provision(t, &GeoIP{DBPath: writeTestDB(t, zurich())})

	if got := resolve(t, g, "[::ffff:81.2.69.200]:9999"); got[FieldCountryCode] != "CH" {
		t.Errorf("mapped IPv6: country_code = %q, want CH", got[FieldCountryCode])
	}
	if got := resolve(t, g, "[fe80::1%25eth0]:9999"); got[FieldCountryCode] != "" {
		t.Errorf("zoned link-local: country_code = %q, want empty", got[FieldCountryCode])
	}
	// A RemoteAddr that is not an address at all must degrade to the empty
	// set, never to a panic.
	if got := resolve(t, g, "not-an-address"); got[FieldCountryCode] != "" {
		t.Errorf("garbage RemoteAddr: country_code = %q, want empty", got[FieldCountryCode])
	}
}

// The point of the task: the database changes on disk and the running plugin
// picks it up, without a restart.
func TestReloadAtRuntime(t *testing.T) {
	path := writeTestDB(t, zurich())

	g := provision(t, &GeoIP{
		DBPath:          path,
		ReloadFrequency: caddy.Duration(20 * time.Millisecond),
	})

	if got := resolve(t, g, "81.2.69.200:1"); got[FieldCity] != "Zurich" {
		t.Fatalf("before reload: city = %q", got[FieldCity])
	}

	bern := zurich()
	bern.CityName = map[string]string{"en": "Bern"}
	bern.RegionCode = "BE"
	bern.PostalCode = "3011"
	replaceAtomically(t, path, buildTestDB(t, bern))

	deadline := time.Now().Add(5 * time.Second)
	for {
		got := resolve(t, g, "81.2.69.200:1")
		if got[FieldCity] == "Bern" {
			if got[FieldRegionCode] != "BE" || got[FieldPostalCode] != "3011" {
				t.Errorf("partial reload: region_code = %q, postal_code = %q", got[FieldRegionCode], got[FieldPostalCode])
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("database was not reloaded within the deadline, city is still %q", got[FieldCity])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A file that does not change must not be re-opened; otherwise a short
// reload_frequency would re-read ~60 MB every tick.
func TestReloadSkipsUnchangedFile(t *testing.T) {
	g := provision(t, &GeoIP{DBPath: writeTestDB(t, zurich())})

	g.db.mu.RLock()
	first := g.db.reader
	g.db.mu.RUnlock()

	if err := g.db.reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}

	g.db.mu.RLock()
	second := g.db.reader
	g.db.mu.RUnlock()

	if first != second {
		t.Error("reload re-opened an unchanged file")
	}
}

func TestMissingDatabaseIsAStartupError(t *testing.T) {
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	defer cancel()

	g := &GeoIP{DBPath: t.TempDir() + "/absent.mmdb"}
	if err := g.Provision(ctx); err == nil {
		t.Fatal("expected Provision to fail without a database and without download_url")
	}
}

// Two handlers on the same path share one open database — that is what keeps a
// config reload from re-opening the file.
func TestSharedDatabaseAcrossHandlers(t *testing.T) {
	path := writeTestDB(t, zurich())

	first := provision(t, &GeoIP{DBPath: path})
	second := provision(t, &GeoIP{DBPath: path})

	if first.db != second.db {
		t.Error("handlers on the same db_path did not share a database")
	}
}

func TestDownloadFormats(t *testing.T) {
	raw := buildTestDB(t, zurich())

	cases := map[string]func(*testing.T, []byte) []byte{
		"tar.gz": tarGz,
		"gz":     gzipBytes,
		"plain":  func(_ *testing.T, b []byte) []byte { return b },
	}

	for name, pack := range cases {
		t.Run(name, func(t *testing.T) {
			body := pack(t, raw)

			var gotUser, gotPass string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotUser, gotPass, _ = r.BasicAuth()
				w.Write(body)
			}))
			defer srv.Close()

			g := provision(t, &GeoIP{
				DBPath:      t.TempDir() + "/GeoLite2-City.mmdb",
				DownloadURL: srv.URL,
				AccountID:   "123456",
				LicenseKey:  "secret",
			})

			if gotUser != "123456" || gotPass != "secret" {
				t.Errorf("basic auth = %q / %q", gotUser, gotPass)
			}
			if got := resolve(t, g, "81.2.69.200:1"); got[FieldCountryCode] != "CH" {
				t.Errorf("country_code = %q after download", got[FieldCountryCode])
			}
			if _, err := os.Stat(g.DBPath); err != nil {
				t.Errorf("database was not written to db_path: %v", err)
			}
		})
	}
}

// A corrupt or error-page response must never replace a working database.
func TestDownloadRejectsInvalidPayload(t *testing.T) {
	good := writeTestDB(t, zurich())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("<html>rate limit exceeded</html>"))
	}))
	defer srv.Close()

	g := provision(t, &GeoIP{DBPath: good, DownloadURL: srv.URL})

	if err := g.db.download(context.Background()); err == nil {
		t.Fatal("expected the download to be rejected")
	}
	if got := resolve(t, g, "81.2.69.200:1"); got[FieldCountryCode] != "CH" {
		t.Errorf("working database was replaced: country_code = %q", got[FieldCountryCode])
	}
}

// A non-City database is the subtler corruption: it opens fine, but resolves
// nothing below country level. verify() has to catch it before the rename.
func TestDownloadRejectsWrongDatabaseType(t *testing.T) {
	path := t.TempDir() + "/GeoLite2-Country.mmdb"
	if err := os.WriteFile(path, buildTestDBOfType(t, "GeoLite2-Country", zurich()), 0o644); err != nil {
		t.Fatal(err)
	}

	err := verify(path)
	if err == nil {
		t.Fatal("expected verify to reject a non-City database")
	}
	if !strings.Contains(err.Error(), "GeoLite2-Country") {
		t.Errorf("error should name the offending type, got: %v", err)
	}
}

func TestHTTPErrorIsReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "invalid license key", http.StatusUnauthorized)
	}))
	defer srv.Close()

	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	defer cancel()

	g := &GeoIP{DBPath: t.TempDir() + "/absent.mmdb", DownloadURL: srv.URL}
	err := g.Provision(ctx)
	if err == nil {
		t.Fatal("expected Provision to fail on a 401")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "invalid license key") {
		t.Errorf("error should name status and body, got: %v", err)
	}
}
