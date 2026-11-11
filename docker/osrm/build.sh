#!/bin/sh
# Step 2 (osrm-build service, official OSRM image): builds the routing data
# for the car and foot profiles (MLD pipeline) from /data/region.osm.pbf.
# A profile is rebuilt only when region.osm.pbf is newer than its last build.
set -eu

for p in car foot; do
  dir="/data/$p"
  if [ -f "$dir/built" ] && [ "$dir/built" -nt /data/region.osm.pbf ]; then
    echo "osrm-build: $p is up to date"
    continue
  fi
  echo "osrm-build: building $p"
  rm -rf "$dir.tmp"
  mkdir -p "$dir.tmp"
  cp /data/region.osm.pbf "$dir.tmp/region.osm.pbf"
  osrm-extract -p "/opt/$p.lua" "$dir.tmp/region.osm.pbf"
  osrm-partition "$dir.tmp/region.osrm"
  osrm-customize "$dir.tmp/region.osrm"
  rm -f "$dir.tmp/region.osm.pbf"
  touch "$dir.tmp/built"
  # Swap in atomically so a running osrm-routed never sees half the files.
  rm -rf "$dir.old"
  [ -d "$dir" ] && mv "$dir" "$dir.old"
  mv "$dir.tmp" "$dir"
  rm -rf "$dir.old"
done
