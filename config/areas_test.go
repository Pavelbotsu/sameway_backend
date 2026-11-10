package config

import "testing"

func TestParseAreasAndAreaFor(t *testing.T) {
	areas, err := ParseAreas(DefaultServiceAreas)
	if err != nil || len(areas) != 2 {
		t.Fatalf("default areas: %v %v", areas, err)
	}
	c := Config{ServiceAreas: areas}
	cases := []struct {
		name     string
		lat, lng float64
		want     string
	}{
		{"Kraków Main Square", 50.0617, 19.9373, "krakow"},
		{"Wieliczka", 49.9870, 20.0647, "krakow"},
		{"Poltava city", 49.5883, 34.5514, "poltava"},
		{"Kremenchuk", 49.0659, 33.4200, "poltava"},
		{"Warsaw", 52.2297, 21.0122, ""},
		{"Kyiv", 50.4501, 30.5234, ""},
	}
	for _, tc := range cases {
		if got := c.AreaFor(tc.lat, tc.lng); got != tc.want {
			t.Errorf("%s: AreaFor = %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := (Config{}).AreaFor(0, 0); got != "*" {
		t.Errorf("no areas configured: got %q, want *", got)
	}
}

func TestParseAreasRejectsBadInput(t *testing.T) {
	for _, s := range []string{"krakow", "krakow:1,2,3", "x:1,2,a,4", "x:5,5,1,1", ":1,2,3,4"} {
		if _, err := ParseAreas(s); err == nil {
			t.Errorf("ParseAreas(%q) should fail", s)
		}
	}
	if a, err := ParseAreas(""); err != nil || len(a) != 0 {
		t.Errorf("empty string = no limit, got %v %v", a, err)
	}
}
