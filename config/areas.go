package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Area is a named rectangle (WGS84 degrees) where Sameway operates.
type Area struct {
	Name                           string
	MinLat, MinLng, MaxLat, MaxLng float64
}

// Contains reports whether the point lies inside the rectangle.
func (a Area) Contains(lat, lng float64) bool {
	return lat >= a.MinLat && lat <= a.MaxLat && lng >= a.MinLng && lng <= a.MaxLng
}

// DefaultServiceAreas: the launch areas. Kraków plus ~30 km (Wieliczka,
// Skawina, Niepołomice, Zabierzów…) and the whole of Poltava oblast.
const DefaultServiceAreas = "krakow:49.80,19.45,50.33,20.40;poltava:48.70,32.00,50.95,35.60"

// ParseAreas reads "name:minLat,minLng,maxLat,maxLng;name2:…". An empty
// string means no limit (every location allowed).
func ParseAreas(s string) ([]Area, error) {
	var out []Area
	for _, part := range strings.Split(s, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, coords, ok := strings.Cut(part, ":")
		if !ok || strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("service area %q: want name:minLat,minLng,maxLat,maxLng", part)
		}
		f := strings.Split(coords, ",")
		if len(f) != 4 {
			return nil, fmt.Errorf("service area %q: want 4 numbers", part)
		}
		var v [4]float64
		for i, x := range f {
			n, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
			if err != nil {
				return nil, fmt.Errorf("service area %q: %v", part, err)
			}
			v[i] = n
		}
		a := Area{Name: strings.TrimSpace(name), MinLat: v[0], MinLng: v[1], MaxLat: v[2], MaxLng: v[3]}
		if a.MinLat >= a.MaxLat || a.MinLng >= a.MaxLng {
			return nil, fmt.Errorf("service area %q: min must be below max", part)
		}
		out = append(out, a)
	}
	return out, nil
}

// loadServiceAreas: SERVICE_AREAS when set (empty = no limit), otherwise
// the launch areas in production and no limit in dev/tests.
func loadServiceAreas(prod bool) []Area {
	s, set := os.LookupEnv("SERVICE_AREAS")
	if !set && prod {
		s = DefaultServiceAreas
	}
	areas, err := ParseAreas(s)
	if err != nil {
		// Validate() reports it; never silently drop the limit.
		serviceAreasErr = err
		return nil
	}
	return areas
}

var serviceAreasErr error

// AreaFor returns the name of the service area containing the point, "" if
// none. With no areas configured every point is allowed and "*" is returned.
func (c Config) AreaFor(lat, lng float64) string {
	if len(c.ServiceAreas) == 0 {
		return "*"
	}
	for _, a := range c.ServiceAreas {
		if a.Contains(lat, lng) {
			return a.Name
		}
	}
	return ""
}
