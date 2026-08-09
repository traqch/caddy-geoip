package caddygeoip

import (
	"strings"
	"testing"
)

// The download URL ends up in log lines, and Caddy's logs leave the node. A
// license key in a query parameter is a leaked secret — this is the guard.
func TestRedactURL(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		mustNot string
	}{
		{
			name:    "maxmind license key in the query",
			in:      "https://download.maxmind.com/app/geoip_download?edition_id=GeoLite2-City&license_key=abc123secret&suffix=tar.gz",
			want:    "https://download.maxmind.com/app/geoip_download?edition_id=GeoLite2-City&license_key=REDACTED&suffix=tar.gz",
			mustNot: "abc123secret",
		},
		{
			name:    "credentials in userinfo",
			in:      "https://user:hunter2@mirror.internal/GeoLite2-City.mmdb",
			want:    "https://mirror.internal/GeoLite2-City.mmdb",
			mustNot: "hunter2",
		},
		{
			name:    "unknown parameters are treated as secrets",
			in:      "https://mirror.internal/db?token=xyz&edition_id=GeoLite2-City",
			want:    "https://mirror.internal/db?edition_id=GeoLite2-City&token=REDACTED",
			mustNot: "xyz",
		},
		{
			name: "plain url is untouched",
			in:   "https://mirror.internal/GeoLite2-City.tar.gz",
			want: "https://mirror.internal/GeoLite2-City.tar.gz",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactURL(tc.in)
			if got != tc.want {
				t.Errorf("redactURL() = %q, want %q", got, tc.want)
			}
			if tc.mustNot != "" && strings.Contains(got, tc.mustNot) {
				t.Errorf("redactURL() leaked %q", tc.mustNot)
			}
		})
	}
}

func TestIsTar(t *testing.T) {
	head := make([]byte, tarPeekSize)
	copy(head[tarMagicOffset:], "ustar")
	if !isTar(head) {
		t.Error("tar header not recognised")
	}

	if isTar(make([]byte, tarPeekSize)) {
		t.Error("zeroed block recognised as tar")
	}
	if isTar([]byte("too short")) {
		t.Error("short buffer recognised as tar")
	}
}
