package services

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Pavelbotsu/sameway_backend/config"
	"github.com/Pavelbotsu/sameway_backend/db"
)

// Static GTFS stop loader (Round 2 §1). Feeds the transit-awareness branch of
// the partial-relay feasibility check: a relay dropoff is offerable when a
// stop sits within GTFSStopRadiusM of the route end. The whole subsystem is
// optional — with GTFS_STATIC_URL unset (or the download failing) the
// transit_stops table stays empty and relay falls back to second-leg driver
// density alone.

// gtfsHTTPClient bounds the feed download; city feeds are a few MB to a few
// tens of MB.
var gtfsHTTPClient = &http.Client{Timeout: 120 * time.Second}

// StartGTFSLoader runs the refresh check in a detached, panic-isolated
// goroutine so a malformed feed can never take the boot path down.
func StartGTFSLoader() {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("panic in gtfs-loader: %v", r)
			}
		}()
		if err := RefreshGTFSStopsIfStale(); err != nil {
			log.Printf("gtfs loader: %v", err)
		}
	}()
}

// RefreshGTFSStopsIfStale downloads and imports stops.txt when the table is
// empty or the newest import is older than GTFSRefreshDays. No-op without a
// configured URL.
func RefreshGTFSStopsIfStale() error {
	if config.C.GTFSStaticURL == "" {
		return nil
	}
	ctx := context.Background()
	var newest *time.Time
	if err := db.DB.QueryRow(ctx,
		`SELECT MAX(imported_at) FROM transit_stops`,
	).Scan(&newest); err != nil {
		return fmt.Errorf("staleness check: %w", err)
	}
	if newest != nil && time.Since(*newest) < time.Duration(config.C.GTFSRefreshDays)*24*time.Hour {
		return nil // fresh enough
	}
	n, err := ImportGTFSStops(config.C.GTFSStaticURL)
	if err != nil {
		return err
	}
	log.Printf("gtfs: imported %d stops from %s", n, config.C.GTFSStaticURL)
	return nil
}

// ImportGTFSStops downloads a GTFS zip and upserts every stops.txt row that
// has coordinates. Returns the number of stops written.
func ImportGTFSStops(url string) (int, error) {
	resp, err := gtfsHTTPClient.Get(url)
	if err != nil {
		return 0, fmt.Errorf("gtfs download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("gtfs download: http %d", resp.StatusCode)
	}
	// zip needs random access — buffer the archive in memory.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return 0, fmt.Errorf("gtfs read: %w", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return 0, fmt.Errorf("gtfs unzip: %w", err)
	}
	for _, f := range zr.File {
		if f.Name != "stops.txt" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return 0, fmt.Errorf("gtfs stops.txt open: %w", err)
		}
		defer rc.Close()
		return importStopsCSV(rc)
	}
	return 0, fmt.Errorf("gtfs: no stops.txt in feed")
}

func importStopsCSV(r io.Reader) (int, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1 // GTFS files vary in trailing optional columns
	header, err := cr.Read()
	if err != nil {
		return 0, fmt.Errorf("gtfs header: %w", err)
	}
	col := map[string]int{}
	for i, h := range header {
		// Strip a UTF-8 BOM that many feeds carry on the first header cell.
		col[strings.TrimPrefix(h, "\uFEFF")] = i
	}
	idIdx, ok1 := col["stop_id"]
	nameIdx, ok2 := col["stop_name"]
	latIdx, ok3 := col["stop_lat"]
	lonIdx, ok4 := col["stop_lon"]
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return 0, fmt.Errorf("gtfs stops.txt: missing required columns")
	}

	ctx := context.Background()
	count := 0
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			// Skip malformed lines rather than abandoning the import.
			continue
		}
		if len(rec) <= idIdx || len(rec) <= nameIdx || len(rec) <= latIdx || len(rec) <= lonIdx {
			continue
		}
		lat, errLat := strconv.ParseFloat(rec[latIdx], 64)
		lon, errLon := strconv.ParseFloat(rec[lonIdx], 64)
		if errLat != nil || errLon != nil || rec[idIdx] == "" {
			continue
		}
		if _, err := db.DB.Exec(ctx, `
			INSERT INTO transit_stops (stop_id, name, location, imported_at)
			VALUES ($1, $2, ST_MakePoint($4, $3)::geography, NOW())
			ON CONFLICT (stop_id) DO UPDATE SET
				name = EXCLUDED.name,
				location = EXCLUDED.location,
				imported_at = NOW()
		`, rec[idIdx], rec[nameIdx], lat, lon); err != nil {
			return count, fmt.Errorf("gtfs upsert %s: %w", rec[idIdx], err)
		}
		count++
	}
	return count, nil
}
