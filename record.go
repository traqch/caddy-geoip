package caddygeoip

import "strconv"

// Placeholder names, without the `traq.geoip.` prefix. Deliberately eight and
// no more: geohash, the resolved address itself and the accuracy radius are
// common in other GeoIP middlewares, but nothing downstream consumes them, and
// an unused header is one more thing to keep correct.
const (
	FieldCountryCode = "country_code"
	FieldCountryName = "country_name"
	FieldRegionCode  = "region_code"
	FieldRegionName  = "region_name"
	FieldCity        = "city"
	FieldPostalCode  = "postal_code"
	FieldLatitude    = "latitude"
	FieldLongitude   = "longitude"
)

// Fields is the placeholder order used everywhere — on a miss the same set is
// emitted with empty values, so a caller never sees a partially populated
// request.
var Fields = []string{
	FieldCountryCode,
	FieldCountryName,
	FieldRegionCode,
	FieldRegionName,
	FieldCity,
	FieldPostalCode,
	FieldLatitude,
	FieldLongitude,
}

// cityRecord is the subset of the GeoLite2-City schema this plugin decodes.
// Coordinates are pointers on purpose: a missing location must stay empty
// rather than collapse to 0/0, which is a real place in the Gulf of Guinea.
type cityRecord struct {
	City struct {
		Names map[string]string `maxminddb:"names"`
	} `maxminddb:"city"`
	Country struct {
		ISOCode string            `maxminddb:"iso_code"`
		Names   map[string]string `maxminddb:"names"`
	} `maxminddb:"country"`
	Location struct {
		Latitude  *float64 `maxminddb:"latitude"`
		Longitude *float64 `maxminddb:"longitude"`
	} `maxminddb:"location"`
	Postal struct {
		Code string `maxminddb:"code"`
	} `maxminddb:"postal"`
	Subdivisions []struct {
		ISOCode string            `maxminddb:"iso_code"`
		Names   map[string]string `maxminddb:"names"`
	} `maxminddb:"subdivisions"`
}

// values flattens the record into the placeholder set. languages is tried in
// order; the first one present on a given name map wins.
func (rec *cityRecord) values(languages []string) map[string]string {
	out := make(map[string]string, len(Fields))
	for _, f := range Fields {
		out[f] = ""
	}

	out[FieldCountryCode] = rec.Country.ISOCode
	out[FieldCountryName] = pickName(rec.Country.Names, languages)
	out[FieldCity] = pickName(rec.City.Names, languages)
	out[FieldPostalCode] = rec.Postal.Code

	// GeoLite2 orders subdivisions broadest-first; the first entry is the
	// canton/state, which is what consumers expect as `region`.
	if len(rec.Subdivisions) > 0 {
		out[FieldRegionCode] = rec.Subdivisions[0].ISOCode
		out[FieldRegionName] = pickName(rec.Subdivisions[0].Names, languages)
	}

	if rec.Location.Latitude != nil {
		out[FieldLatitude] = formatCoord(*rec.Location.Latitude)
	}
	if rec.Location.Longitude != nil {
		out[FieldLongitude] = formatCoord(*rec.Location.Longitude)
	}

	return out
}

// emptyValues is the miss case: every placeholder present, all empty. An empty
// header is conventionally read as absent, so this lands as "no geo" rather
// than as bogus geo.
func emptyValues() map[string]string {
	out := make(map[string]string, len(Fields))
	for _, f := range Fields {
		out[f] = ""
	}
	return out
}

func pickName(names map[string]string, languages []string) string {
	for _, lang := range languages {
		if v, ok := names[lang]; ok && v != "" {
			return v
		}
	}
	return ""
}

// formatCoord keeps the shortest representation that round-trips, which is
// what any downstream float parser expects.
func formatCoord(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}
