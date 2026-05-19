package services

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/Pavelbotsu/sameway_backend/config"
)

type RouteResult struct {
	WKT        string
	Coords     [][2]float64
	DistanceKm float64
}

func GetRoadRoute(originLat, originLng, destLat, destLng float64) (*RouteResult, error) {
	url := fmt.Sprintf(
		"%s/route/v1/driving/%f,%f;%f,%f?overview=full&geometries=geojson",
		config.C.OSRMBaseURL, originLng, originLat, destLng, destLat,
	)

	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("osrm request: %w", err)
	}
	defer resp.Body.Close()

	var osrm struct {
		Routes []struct {
			Distance float64 `json:"distance"`
			Geometry struct {
				Coordinates [][2]float64 `json:"coordinates"`
			} `json:"geometry"`
		} `json:"routes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&osrm); err != nil {
		return nil, fmt.Errorf("osrm decode: %w", err)
	}
	if len(osrm.Routes) == 0 {
		return nil, fmt.Errorf("no route found")
	}

	route := osrm.Routes[0]
	coords := route.Geometry.Coordinates
	parts := make([]string, len(coords))
	for i, c := range coords {
		parts[i] = fmt.Sprintf("%f %f", c[0], c[1])
	}
	wkt := "LINESTRING(" + strings.Join(parts, ", ") + ")"

	return &RouteResult{
		WKT:        wkt,
		Coords:     coords,
		DistanceKm: route.Distance / 1000,
	}, nil
}
