package caddygeoip

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
)

// The fixtures are built at test time rather than committed: a binary blob in
// the repo would be unreviewable, and GeoLite2 data is licensed.

// testCity describes one entry of the fixture database. A zero-valued field is
// simply left out of the record, which is how a real GeoLite2 entry looks when
// MaxMind has no data for it.
type testCity struct {
	Network     string // CIDR
	CountryCode string
	CountryName map[string]string
	RegionCode  string
	RegionName  map[string]string
	CityName    map[string]string
	PostalCode  string
	Latitude    *float64
	Longitude   *float64
}

func f64(v float64) *float64 { return &v }

// zurich is the full-record case: every one of the eight fields populated.
func zurich() testCity {
	return testCity{
		Network:     "81.2.69.192/28",
		CountryCode: "CH",
		CountryName: map[string]string{"en": "Switzerland", "de": "Schweiz"},
		RegionCode:  "ZH",
		RegionName:  map[string]string{"en": "Zurich", "de": "Zürich"},
		CityName:    map[string]string{"en": "Zurich", "de": "Zürich"},
		PostalCode:  "8001",
		Latitude:    f64(47.3667),
		Longitude:   f64(8.55),
	}
}

// countryOnly is the sparse case: GeoLite2 resolves the country but nothing
// below it. Region, city, postal and coordinates must stay empty — in
// particular the coordinates must not collapse to 0/0.
//
// The network is one of MaxMind's own test ranges. The documentation ranges
// (198.51.100.0/24, 203.0.113.0/24) are unusable here: mmdbwriter refuses to
// insert into reserved space.
func countryOnly() testCity {
	return testCity{
		Network:     "89.160.20.112/28",
		CountryCode: "SG",
		CountryName: map[string]string{"en": "Singapore"},
	}
}

func (c testCity) record() mmdbtype.Map {
	rec := mmdbtype.Map{}

	if c.CountryCode != "" || len(c.CountryName) > 0 {
		country := mmdbtype.Map{}
		if c.CountryCode != "" {
			country["iso_code"] = mmdbtype.String(c.CountryCode)
		}
		if names := namesMap(c.CountryName); names != nil {
			country["names"] = names
		}
		rec["country"] = country
	}

	if c.RegionCode != "" || len(c.RegionName) > 0 {
		sub := mmdbtype.Map{}
		if c.RegionCode != "" {
			sub["iso_code"] = mmdbtype.String(c.RegionCode)
		}
		if names := namesMap(c.RegionName); names != nil {
			sub["names"] = names
		}
		rec["subdivisions"] = mmdbtype.Slice{sub}
	}

	if names := namesMap(c.CityName); names != nil {
		rec["city"] = mmdbtype.Map{"names": names}
	}

	if c.PostalCode != "" {
		rec["postal"] = mmdbtype.Map{"code": mmdbtype.String(c.PostalCode)}
	}

	if c.Latitude != nil || c.Longitude != nil {
		loc := mmdbtype.Map{}
		if c.Latitude != nil {
			loc["latitude"] = mmdbtype.Float64(*c.Latitude)
		}
		if c.Longitude != nil {
			loc["longitude"] = mmdbtype.Float64(*c.Longitude)
		}
		rec["location"] = loc
	}

	return rec
}

func namesMap(in map[string]string) mmdbtype.Map {
	if len(in) == 0 {
		return nil
	}
	out := mmdbtype.Map{}
	for lang, name := range in {
		out[mmdbtype.String(lang)] = mmdbtype.String(name)
	}
	return out
}

// buildTestDB writes a GeoLite2-City-shaped database containing cities and
// returns its bytes.
func buildTestDB(t *testing.T, cities ...testCity) []byte {
	t.Helper()
	return buildTestDBOfType(t, "GeoLite2-City", cities...)
}

// buildTestDBOfType exists so the "wrong database type" case can be built the
// same way as the good one.
func buildTestDBOfType(t *testing.T, dbType string, cities ...testCity) []byte {
	t.Helper()

	writer, err := mmdbwriter.New(mmdbwriter.Options{
		DatabaseType: dbType,
		RecordSize:   24,
		Languages:    []string{"en", "de"},
	})
	if err != nil {
		t.Fatalf("mmdbwriter.New: %v", err)
	}

	for _, city := range cities {
		_, network, err := net.ParseCIDR(city.Network)
		if err != nil {
			t.Fatalf("parse %s: %v", city.Network, err)
		}
		if err := writer.Insert(network, city.record()); err != nil {
			t.Fatalf("insert %s: %v", city.Network, err)
		}
	}

	var buf bytes.Buffer
	if _, err := writer.WriteTo(&buf); err != nil {
		t.Fatalf("write mmdb: %v", err)
	}
	return buf.Bytes()
}

// writeTestDB writes a fixture to a fresh temp directory and returns its path.
func writeTestDB(t *testing.T, cities ...testCity) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "GeoLite2-City.mmdb")
	if err := os.WriteFile(path, buildTestDB(t, cities...), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// replaceAtomically swaps the database file the way every real updater has to:
// write beside it, then rename. Overwriting in place would truncate the file
// the running reader has memory-mapped, and lookups would fail mid-flight with
// "unexpected end of database" until the reload tick caught up.
func replaceAtomically(t *testing.T, path string, content []byte) {
	t.Helper()
	tmp := path + ".next"
	if err := os.WriteFile(tmp, content, 0o644); err != nil {
		t.Fatalf("write %s: %v", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatalf("rename onto %s: %v", path, err)
	}
}

// tarGz packs raw as MaxMind does: a gzipped tarball with the .mmdb nested in
// a dated directory, next to the files that must be skipped.
func tarGz(t *testing.T, raw []byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	entries := []struct {
		name string
		body []byte
	}{
		{"GeoLite2-City_20260809/COPYRIGHT.txt", []byte("test fixture")},
		{"GeoLite2-City_20260809/GeoLite2-City.mmdb", raw},
	}
	for _, e := range entries {
		if err := tw.WriteHeader(&tar.Header{
			Name:     e.name,
			Mode:     0o644,
			Size:     int64(len(e.body)),
			Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatalf("tar header %s: %v", e.name, err)
		}
		if _, err := tw.Write(e.body); err != nil {
			t.Fatalf("tar write %s: %v", e.name, err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

func gzipBytes(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(raw); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}
