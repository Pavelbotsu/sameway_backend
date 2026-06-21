# Sameway — Driver ↔ Passenger Matching

How the backend pairs drivers and passengers in real time. Sameway is a
*same-way* carpool: a driver keeps their own route and never detours, so a
passenger is matched to a point on that route, picked up there, and walks the
last stretch to their real destination.

> **TL;DR** — Two "intent" tables (`active_drivers`, `active_passengers`) are
> joined through PostGIS geometry into a "handshake" table (`ride_requests`).
> Matches form three ways: the driver pushes offers along their **corridor**,
> the passenger pulls a list of **nearby** drivers, or either browses **planned**
> future trips. Both real-time paths converge on one race-safe seat-reservation
> transaction. An optional AI layer only reorders results.

---

## Table of contents

1. [Data model](#1-data-model)
2. [The three discovery mechanisms](#2-the-three-discovery-mechanisms)
3. [Mechanism A — driver-initiated corridor match](#3-mechanism-a--driver-initiated-corridor-match)
4. [Mechanism B — passenger-initiated nearby search](#4-mechanism-b--passenger-initiated-nearby-search)
5. [Mechanism C — planned future trips](#5-mechanism-c--planned-future-trips)
6. [Ranking layer (geometric + AI)](#6-ranking-layer-geometric--ai)
7. [The shared accept core (seat reservation)](#7-the-shared-accept-core-seat-reservation)
8. [Idempotency & deduplication](#8-idempotency--deduplication)
9. [Lifecycle rules that shape matching](#9-lifecycle-rules-that-shape-matching)
10. [Combination matrix](#10-combination-matrix)
11. [Tunables reference](#11-tunables-reference)

---

## 1. Data model

All geographic columns are PostGIS `GEOGRAPHY` (SRID 4326, WGS-84), so
`ST_DWithin(...)` distances are in **meters**.

| Table | One row per | Key fields |
|---|---|---|
| `active_drivers` | live driver (unique `driver_id`) | `route` (LINESTRING from OSRM), `origin`, `destination`, `current_location`, `corridor_km`, `seats`, `seats_taken`, `status` (`looking` / `full`), `created_at` |
| `active_passengers` | live passenger | `location` (point), `destination_lat` / `destination_lng` (optional), `updated_at` |
| `ride_requests` | one pairing attempt | `driver_id`, `passenger_id`, `pickup_location`, `status`, `pickup_code`, `created_at` |
| `planned_trips` | one future scheduled trip | origin / destination / route / `planned_at` / `status` |

**`ride_requests` status machine:**

```
pending ──accept──▶ accepted ──pickup confirmed──▶ in_progress ──drop-off──▶ done
   │                   │
   └─decline/timeout   └─cancel
        ▼                   ▼
     declined           cancelled
```

---

## 2. The three discovery mechanisms

```
(A) Driver-initiated corridor fan-out   →  server PUSHES offers to passengers  →  passenger accepts
(B) Passenger-initiated nearby search   →  passenger PULLS a list, picks one    →  driver accepts
(C) Planned-trips search (future rides) →  browse board, no real-time handshake

        (A) and (B) both funnel into ONE seat-reservation transaction  (§7)
```

The difference between A and B is **who chooses and who confirms**:

- **A:** the driver defines a route; the server finds passengers along it and
  pushes them offers; the **passenger** confirms.
- **B:** the passenger sees nearby drivers and requests one; the **driver**
  confirms.

---

## 3. Mechanism A — driver-initiated corridor match

This is the core algorithm. *File: [`services/geo.go`](services/geo.go) →
`FindPassengersInCorridor`; orchestration in [`handlers/driver.go`](handlers/driver.go).*

**Trigger**

- Driver calls `POST /driver/route` (`SetRoute`), **or**
- Driver moves → `PUT /driver/location` fires a re-run, throttled to once per
  **30 s per driver** (`rerunCorridorMatch`).

**Steps**

1. **Build the road route** — OSRM `driving` profile, origin → destination,
   producing a `LINESTRING` ([`services/routing.go`](services/routing.go)).
2. **Store the driver** — `UpsertActiveDriver` (one active route per driver; an
   upsert resets `seats_taken = 0`, `status = 'looking'`).
3. **Run the heading-aware corridor query** (per candidate passenger):

   | Value | How it's computed | Meaning |
   |---|---|---|
   | `pax_frac` | `ST_LineLocatePoint(route, passenger)` | passenger's position along route (0→1) |
   | `driver_frac` | same for `current_location` (or `origin`) | driver's position along route |
   | `pickup` | `ST_ClosestPoint(route, passenger)` | **projection of passenger onto the road** (no-detour pickup) |
   | `walk_m` | `ST_Distance(passenger, pickup)` | how far the passenger walks to the pickup |
   | `driver_to_pickup_m` | `(pax_frac − driver_frac) × route_len` | signed; **negative = passenger is behind the driver** |

   **Pre-filters**
   - `ST_DWithin(passenger, route, GREATEST(corridor_km·1000, relaxed_walk))`
   - `updated_at > NOW() − 10 min` (passenger is online/fresh)
   - `NOT EXISTS` a `pending`/`accepted` request for that passenger
     (**one active request per passenger, globally**)

   **Direction filter** — `pax_frac > driver_frac + tolerance`: the passenger
   must be **ahead** of the driver.

   **Inclusion — a disjunction of two branches:**

   - **Strict corridor:** `walk < corridor_km·1000` **and** `walk < 1000 m`
     **and** `driver_to_pickup < 2000 m` **and**
     `(passenger declared no destination OR their destination is within 1.5 km of the driver's)`.
   - **Destination-anchored (relaxed):** destination within **1.5 km** of the
     driver's **and** `walk < 5000 m` **and** `driver_to_pickup < 10000 m`.

   Each match is tagged `match_kind` = `corridor` or `destination_anchored` so
   the app can show a "longer walk, same destination" banner.

   **Order & cap:** `ORDER BY driver_to_pickup_m ASC LIMIT MAX_CORRIDOR_NOTIFICATIONS`
   (default 20) — nearest-ahead first.

4. **Rank** (optional reorder) — see [§6](#6-ranking-layer-geometric--ai).
5. **Reserve + notify** — for each match, `CreateRideRequest` inserts a
   `pending` row (idempotent); if it's genuinely new, send a `ride_request`
   WebSocket event + FCM push carrying the pickup point, `walk_distance_m`,
   `driver_distance_to_pickup_m`, `match_method`, and `match_kind`.
6. **Passenger decides** — `POST /passenger/respond` (→ [§7](#7-the-shared-accept-core-seat-reservation)).

> ⚠️ **Key subtlety.** In the strict branch the clause
> `(destination_lat IS NULL OR dest_match)` means a passenger who declared a
> destination that does **not** match the driver's is **excluded**. Declaring a
> destination is therefore a *filter* (only same-way drivers), not just a hint.

---

## 4. Mechanism B — passenger-initiated nearby search

*File: [`services/geo.go`](services/geo.go) → `GetNearbyDrivers`;
[`handlers/passenger.go`](handlers/passenger.go).*

**Trigger** — `POST /passenger/nearby-drivers`, polled by the map. The client
always sends `radius_km` (default 10); when a destination is set it also sends
`dest_lat/lng` + `dest_radius_km` (1 km).

**Query** returns drivers where:
- `origin` is within `radius_km` of the passenger, **and**
- if `dest_radius_km > 0`: the driver's **route passes within `dest_radius_km`**
  of the passenger's destination, **and**
- `status = 'looking'` **and** `created_at > NOW() − 30 min` **and** not the
  caller themselves.

The passenger taps a driver → `POST /passenger/request-driver` → `CreateRideRequest`
with **pickup = the passenger's raw current location** (note: *not* projected
onto the route, unlike Mechanism A) → `passenger_request` WS + FCM to the
driver. The **driver decides** via `POST /driver/respond`.

---

## 5. Mechanism C — planned future trips

*File: [`services/trips.go`](services/trips.go).*

Entirely separate from real time — a browse-and-contact board.

- Driver publishes via `POST /driver/trips` (`CreatePlannedTrip`).
- Anyone searches via `POST /trips/search` (`SearchPlannedTrips`): filters by
  origin proximity (`ST_DWithin`), optional destination proximity,
  `planned_at > now`, `status = 'open'`; sortable by time / distance /
  destination name; limit 50.

No WebSocket, no seat reservation.

---

## 6. Ranking layer (geometric + AI)

*File: [`services/ranker/`](services/ranker); applied in `handlers/driver.go → rankAndReorder`.*
**Applies to Mechanism A only.**

- Runs only when there are **more than one** candidate.
- `prefer_ai` is a **per-call** flag on `SetRoute`.
  - **off** → `GeometricRanker`: pure passthrough, preserves the SQL order
    (`driver_to_pickup_m ASC`).
  - **on** (and `AI_RANKER_ENABLED=true`) → `AIRanker`.
- **AIRanker** calls an OpenAI-compatible chat endpoint (configured to Groq
  `llama-3.1-8b-instant`). It sends **only anonymized features** —
  `walk_m`, `driver_to_pickup_m`, `avg_rating`, `rating_count`, plus an index.
  **No coordinates, names, or user IDs ever leave the server.** The model
  returns `{index, score, reason}`; matches are re-sorted by score.
- **Failure posture:** any timeout (2 s budget), non-2xx, or malformed JSON →
  fall back to the geometric order, tagged `geometric_fallback`. The method used
  is stamped on the WS payload as `match_method`.

The ranker is a clean `MatchRanker` interface — a third strategy (e.g. a local
ML model) can be added without touching handler code.

---

## 7. The shared accept core (seat reservation)

Both directions converge on an **identical transaction**
(`respondAccept` for passenger-accepts-offer, `driverRespondAccept` for
driver-accepts-request):

```sql
BEGIN
  -- 1. Lock the pending request
  SELECT ... FROM ride_requests
    WHERE id = ? AND status = 'pending' FOR UPDATE;

  -- 2. Lock driver capacity, validate
  SELECT seats, seats_taken FROM active_drivers
    WHERE driver_id = ? AND status IN ('looking','full') FOR UPDATE;
  -- if seats_taken >= seats  →  409 "ride is full"

  -- 3. Reserve a seat
  UPDATE active_drivers
    SET seats_taken = seats_taken + 1,
        status = CASE WHEN seats_taken + 1 >= seats THEN 'full' ELSE 'looking' END;

  -- 4. Accept this request
  UPDATE ride_requests SET status = 'accepted' WHERE id = ?;

  -- 5. Auto-cancel the passenger's OTHER active requests
  UPDATE ride_requests SET status = 'cancelled'
    WHERE passenger_id = ? AND status IN ('pending','accepted') AND id <> ?
    RETURNING id, driver_id;
COMMIT;

-- post-commit: WS + FCM to both parties; assign 4-digit pickup_code;
-- each bumped competitor's driver gets ride_cancelled (cancelled_by = "passenger_matched_elsewhere")
```

Two `FOR UPDATE` row locks make seat reservation race-free; the auto-cancel
guarantees a passenger ends up in **exactly one** ride even if several drivers
had offered. Decline simply flips the row to `declined` and notifies the other
side.

---

## 8. Idempotency & deduplication

`CreateRideRequest` relies on a **partial unique index** on
`(driver_id, passenger_id)` scoped to `status IN ('pending','accepted','in_progress')`:

```sql
INSERT ... ON CONFLICT (driver_id, passenger_id)
  WHERE status IN ('pending','accepted','in_progress')
  DO NOTHING RETURNING id;
```

- new row → `(id, isNew = true)` → send the notification
- existing active row → `(id, isNew = false)` → **suppress** the duplicate

So if a driver's corridor already notified a passenger (A) and that passenger
then taps the same driver (B), no second event fires. Cancelled/declined history
rows live outside the index and coexist freely.

---

## 9. Lifecycle rules that shape matching

| Rule | Effect |
|---|---|
| **Reaper** (every 60 s) | `pending` requests older than `RIDE_REQUEST_TIMEOUT_MINUTES` (5) are cancelled (`cancelled_by = "timeout"`), freeing the passenger's one-active-request slot. |
| **Passenger staleness** | `updated_at` older than 10 min → invisible to corridor matching. |
| **Driver staleness** | `created_at` older than 30 min → drops out of nearby search. |
| **Seat release** | drop-off / cancel / "end day" do `seats_taken = GREATEST(seats_taken − 1, 0)` and flip `full → looking`, re-opening the driver to new matches. |
| **Pickup handshake** (post-match) | a 4-digit `pickup_code` is generated on accept; it is **asymmetric** — only the driver sees it, the passenger must ask verbally and type it, proving a real meeting before the ride flips to `in_progress`. |
| **Pickup geometry** | corridor match → pickup = projection on route; nearby match → pickup = raw passenger location. `GET /pickup-route/:id` returns three OSRM legs (driver→pickup driving, pickup→destination driving, passenger→pickup walking). |

---

## 10. Combination matrix

| Situation | Outcome |
|---|---|
| Driver route + passenger has **no destination** | Legacy "any corridor": strict branch, walk < 1 km & ahead < 2 km. |
| Driver route + destination **≤ 1.5 km** of driver's | Both branches eligible; relaxed allows walk ≤ 5 km / ahead ≤ 10 km, tagged `destination_anchored`. |
| Driver route + destination **far** from driver's | **No match** — both branches require dest match. A mismatched destination filters the driver out. |
| Driver moves along route | Throttled re-run picks up newly-ahead/in-range passengers; idempotent, so already-notified are skipped. |
| Passenger browses, **no destination** | Sees every `looking` driver within 10 km. |
| Passenger browses, **with destination** | Sees only drivers whose route passes within 1 km of the destination. |
| Passenger requests a driver | Driver gets `passenger_request`, accepts via shared core; pickup = passenger location. |
| Driver corridor-notifies a passenger | Passenger gets `ride_request`, accepts via shared core; pickup = projection on route. |
| Same pair via both A and B | Deduped by the partial unique index (`isNew = false` suppresses the 2nd notify). |
| Passenger has offers from several drivers, accepts one | The others auto-cancel; those drivers get `passenger_matched_elsewhere`. |
| Two passengers race for the last seat | `FOR UPDATE` + capacity check → one wins, the other gets 409 "ride is full". |
| AI on vs off | Per-call `prefer_ai`; AI reorders by anonymized features, falls back to geometric on any error. **Driver-initiated path only.** |
| Pending request ignored 5 min | Reaper cancels it (`timeout`). |
| Future trip | Mechanism C — browse only. |

---

## 11. Tunables reference

| Constant / config | Default | Meaning |
|---|---|---|
| `destinationMatchMeters` | 1500 m | how close destinations must be to count as "same way" |
| `maxWalkM` / `relaxedMaxWalkM` | 1000 / 5000 m | walk cap (strict vs destination-anchored) |
| `earlyNotifyRadiusM` / relaxed | 2000 / 10000 m | how far ahead the driver may be |
| `aheadToleranceFraction` | 0.01 | anti-flutter for a driver sitting on the pickup point |
| passenger staleness | 10 min | online window for corridor matching |
| driver staleness | 30 min | online window for nearby search |
| `MAX_CORRIDOR_NOTIFICATIONS` | 20 | fan-out cap per driver |
| `RIDE_REQUEST_TIMEOUT_MINUTES` | 5 | reaper pending TTL |
| ranker timeout | 2 s | AI call budget before geometric fallback |

*Geometry tunables live in [`services/geo.go`](services/geo.go); the rest in
`config` / environment variables.*
