#!/bin/sh
# Step 1 of the self-hosted routing data (run by the osrm-data service).
# Downloads OpenStreetMap extracts from Geofabrik, cuts Poltava oblast out of
# the Ukraine file (Geofabrik has no oblast-level extracts) and merges it with
# Małopolska into /data/region.osm.pbf. Skips when the file exists, unless
# FORCE=1 (monthly refresh: OSRM_REFRESH=1, see README).
#
# Areas must cover config.DefaultServiceAreas (handlers/service_area.go):
#   krakow  49.80,19.45 – 50.33,20.40  ⊂ Małopolska
#   poltava 48.70,32.00 – 50.95,35.60  = the bbox cut below
set -eu

OUT=/data
POLTAVA_BBOX="32.00,48.70,35.60,50.95" # minLng,minLat,maxLng,maxLat

if [ -f "$OUT/region.osm.pbf" ] && [ "${FORCE:-0}" != "1" ]; then
  echo "osrm-data: $OUT/region.osm.pbf present, nothing to do"
  exit 0
fi

apt-get update -qq
apt-get install -y -qq --no-install-recommends osmium-tool wget ca-certificates >/dev/null

mkdir -p "$OUT/src"
cd "$OUT/src"
echo "osrm-data: downloading Małopolska and Ukraine (~1 GB)"
wget -nv -O malopolskie.osm.pbf https://download.geofabrik.de/europe/poland/malopolskie-latest.osm.pbf
wget -nv -O ukraine.osm.pbf https://download.geofabrik.de/europe/ukraine-latest.osm.pbf

echo "osrm-data: cutting Poltava oblast"
osmium extract --bbox "$POLTAVA_BBOX" --strategy complete_ways --overwrite \
  -o poltava.osm.pbf ukraine.osm.pbf
rm -f ukraine.osm.pbf

echo "osrm-data: merging"
osmium merge --overwrite -f pbf -o "$OUT/region.osm.pbf.partial" malopolskie.osm.pbf poltava.osm.pbf
mv "$OUT/region.osm.pbf.partial" "$OUT/region.osm.pbf"
rm -f malopolskie.osm.pbf poltava.osm.pbf
ls -lh "$OUT/region.osm.pbf"
