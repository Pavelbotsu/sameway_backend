# Sameway Backend

Go HTTP + WebSocket API for the Sameway ridesharing app. Drivers set a road route via OSRM; the server matches passengers along that corridor with PostGIS, brokers accept/decline over WebSocket, and records completed trips with CO₂ savings.

---

## Stack

| Layer | Choice |
|---|---|
| Language | Go 1.22 |
| HTTP | [Gin](https://github.com/gin-gonic/gin) |
| WebSocket | [gorilla/websocket](https://github.com/gorilla/websocket) |
| Database | PostgreSQL 15 + **PostGIS** extension |
| DB driver | [pgx/v5](https://github.com/jackc/pgx) (raw SQL, no ORM) |
| Routing engine | [OSRM](https://project-osrm.org/) HTTP API |
| Push | Firebase FCM (optional in dev) |
| Auth | JWT ([golang-jwt/v5](https://github.com/golang-jwt/jwt)), bcrypt passwords |
| Config | `.env` via [godotenv](https://github.com/joho/godotenv) |

---

## Quick start

```bash
# 1. Start PostgreSQL + PostGIS
docker compose -f docker/docker-compose.yml up -d

# 2. Configure environment
cp .env.example .env
# edit at least JWT_SECRET

# 3. Run
go run .
```

Server starts on `:8080`. All tables auto-migrate on startup.

To enable FCM in development, drop a `serviceAccountKey.json` next to `main.go` and point `FIREBASE_CREDENTIALS_PATH` at it. Skip this and FCM silently no-ops.

---

## Environment variables

| Variable | Default | Purpose |
|---|---|---|
| `PORT` | `8080` | HTTP listen port |
| `DB_HOST` | `localhost` | Postgres host |
| `DB_PORT` | `5432` | Postgres port |
| `DB_USER` | `postgres` | Postgres user |
| `DB_PASSWORD` | `password` | Postgres password |
| `DB_NAME` | `rideshare` | Database name |
| `APP_ENV` | `dev` | `prod` refuses to boot on insecure defaults (JWT secret, public OSRM) |
| `JWT_SECRET` | `change-this` | JWT signing secret — **prod refuses to boot with the default** |
| `JWT_TTL_HOURS` | `720` | Token lifetime (hours) |
| `FIREBASE_CREDENTIALS_PATH` | `./serviceAccountKey.json` | Service-account JSON; empty/missing → FCM disabled |
| `OSRM_BASE_URL` | `http://router.project-osrm.org` | OSRM endpoint |
| `MAX_CORRIDOR_NOTIFICATIONS` | `20` | Max passengers notified per driver SetRoute fanout |
| `RIDE_REQUEST_TIMEOUT_MINUTES` | `5` | Reaper cancels `pending` ride_requests older than this |

---

## Folder layout

```
sameway_backend/
├── main.go                ← Gin route wiring, server start, reaper goroutine
├── config/                ← .env → Config struct
├── db/
│   ├── postgres.go        ← pgxpool connect + auto-migrate ALL tables
│   └── redis.go           ← redis.Client connect (optional)
├── models/                ← User, ActiveDriver, RideRequest, status constants
├── middleware/auth.go     ← JWT Auth(), RequireRole(), GenerateToken()
├── handlers/
│   ├── auth.go            ← Register, Login, Profile, SwitchRole, Stats, etc.
│   ├── driver.go          ← SetRoute, UpdateStatus, DeleteRoute, CancelRide
│   ├── passenger.go       ← RespondToRide (seat-reservation tx),
│   │                       NearbyDrivers, RequestDriver, CancelRide
│   ├── chat.go            ← SendMessage, GetMessages
│   ├── trips.go           ← PlanTrip, SearchTrips, planned-trip helpers
│   ├── promotions.go      ← Static promotion list
│   ├── ratings.go         ← SubmitRating (one per ride, idempotent)
│   ├── websocket.go       ← WSHub singleton + /ws upgrade endpoint
│   └── reaper.go          ← Background pending-request sweeper
├── services/
│   ├── routing.go         ← OSRM HTTP call → RouteResult{WKT, coords, dist}
│   ├── geo.go             ← PostGIS queries (UpsertActiveDriver,
│   │                       FindPassengersInCorridor, GetNearbyDrivers, etc.)
│   ├── co2.go             ← RecordCompletedTrip + CO₂ math
│   ├── trips.go           ← Planned-trip helpers
│   └── notify.go          ← Firebase FCM (gated on credentials)
└── docker/docker-compose.yml  ← postgis/postgis:15-3.3 + redis:7-alpine
```

---

## REST API

All routes prefixed with `/`. JWT in `Authorization: Bearer <token>` header except where noted.

### Auth (`/auth`)

| Method | Path | Auth | Body / Result |
|---|---|---|---|
| `POST` | `/register` | — | `{name, email, password, role}` → `{token, user_id, role, name, email}` |
| `POST` | `/login` | — | `{email, password}` → `{token, user_id, role, name, email}` |
| `GET` | `/profile` | ✓ | Profile + avg_rating + rating_count |
| `PUT` | `/profile` | ✓ | `{name?, car_make?, car_model?, car_color?, car_plate?}` |
| `POST` | `/switch-role` | ✓ | `{role}` → new JWT |
| `PUT` | `/fcm-token` | ✓ | `{fcm_token}` |
| `GET` | `/fcm-status` | ✓ | `{fcm_enabled}` |
| `POST` | `/test-notification` | ✓ | Sends test push to own token |
| `GET` | `/stats` | ✓ | CO₂ + trip aggregates |
| `POST` | `/change-password` | ✓ | `{current_password, new_password}` |
| `GET` | `/trips` | ✓ | Last 50 completed trips |

### Ratings (root)

| Method | Path | Auth | Body |
|---|---|---|---|
| `POST` | `/ratings` | ✓ | `{rated_id, ride_request_id, stars, comment?}` — idempotent (`ON CONFLICT DO NOTHING`) |

### Driver (`/driver`, role:driver)

| Method | Path | Description |
|---|---|---|
| `POST` | `/route` | OSRM route + corridor query + capped fanout to passengers |
| `PUT` | `/location` | Live position (PostGIS `current_location` + WS relay) |
| `GET` | `/requests` | Pending/accepted/declined requests for this driver |
| `PUT` | `/status` | `{status, distance_km?}` — on `done` records trips, resets seats |
| `DELETE` | `/route` | Cancels active route + notifies all affected passengers |
| `DELETE` | `/ride/:request_id` | Cancels one accepted request + releases the seat |
| `POST` | `/trips` | Plan a future trip |
| `GET` | `/trips` | Own planned trips |
| `DELETE` | `/trips/:id` | Cancel planned trip |

### Passenger (`/passenger`, role:passenger)

| Method | Path | Description |
|---|---|---|
| `PUT` | `/location` | Position for corridor matching |
| `POST` | `/respond` | `{request_id, status: accepted\|declined}` — **transactional seat reservation** for `accepted` |
| `POST` | `/nearby-drivers` | List drivers near `(lat,lng)` with optional `dest_lat/lng/radius` |
| `POST` | `/request-driver` | Initiate a request to a specific driver |
| `DELETE` | `/active` | Go offline |
| `DELETE` | `/ride/:request_id` | Cancel + release seat + notify driver |

### Chat (`/chat`, auth required)

| Method | Path | Description |
|---|---|---|
| `POST` | `/send` | `{request_id, content}` → WS chat_message |
| `GET` | `/:request_id` | Message history |

### Public / WS

| Method | Path | Description |
|---|---|---|
| `POST` | `/trips/search` | Searches planned trips by proximity |
| `GET` | `/promotions` | Static promotion list |
| `GET` | `/ws?token=<jwt>` | WebSocket upgrade (token via query because browsers can't set WS headers) |

---

## Matching algorithm

The system uses a **two-sided fan-out model** built on PostGIS, not Redis GEO or geohashes.

### Driver-initiated (the "corridor" flow)

1. Driver POSTs `/driver/route` with origin + destination + corridor_km + seats.
2. `services.GetRoadRoute` calls OSRM → WKT `LINESTRING` of the road path.
3. `services.UpsertActiveDriver` stores the route; `seats_taken=0`, `status='looking'`.
4. `services.FindPassengersInCorridor` runs a PostGIS query:
   - `ST_DWithin(passenger_point::geography, route_line::geography, corridor_km*1000)`
   - excludes passengers who are already `pending` or `accepted` elsewhere (`NOT EXISTS` clause)
   - excludes passengers whose last location update is older than 10 min
   - `ORDER BY ST_Distance(...) ASC LIMIT MAX_CORRIDOR_NOTIFICATIONS`
5. For each matched passenger: insert `ride_requests` (status `'pending'`), send FCM, fire `ride_request` WS with the driver's name, car info, avg rating, request_id.

### Passenger-initiated

`POST /passenger/nearby-drivers` returns drivers via two ST_DWithin clauses: origin proximity (`radius_km`) AND optional route-passes-near-destination (`dest_radius_km`). Client picks one; `POST /passenger/request-driver` creates a single `ride_request` and fires `passenger_request` WS to the chosen driver.

### Accept-path concurrency

`RespondToRide` for `status='accepted'` runs inside a pgx transaction:

```text
BEGIN
  SELECT driver_id FROM ride_requests
    WHERE id=$1 AND passenger_id=$2 AND status='pending'
    FOR UPDATE
  SELECT seats, seats_taken FROM active_drivers
    WHERE driver_id=$1 AND status IN ('looking','full')
    FOR UPDATE
  -- if seats_taken >= seats → 409 ride is full
  UPDATE active_drivers SET seats_taken=seats_taken+1,
         status=CASE WHEN seats_taken+1>=seats THEN 'full' ELSE 'looking' END
  UPDATE ride_requests SET status='accepted' WHERE id=$1
  UPDATE ride_requests SET status='cancelled'
    WHERE passenger_id=$1 AND status IN ('pending','accepted') AND id<>$2
    RETURNING id, driver_id
COMMIT
```

Each auto-cancelled competing request fires a `ride_cancelled` WS event to its driver with `cancelled_by: "passenger_matched_elsewhere"`.

`PassengerCancelRide` / `DriverCancelRide` / `UpdateStatus 'done'` each release seats with `seats_taken = GREATEST(seats_taken-1, 0)` and flip `'full'→'looking'` where appropriate.

### Reaper goroutine

`handlers.ReapPendingRideRequests` (spawned in `main.go` before `r.Run`) ticks every 60 s:

```sql
UPDATE ride_requests SET status='cancelled'
  WHERE status='pending'
    AND created_at < NOW() - ($1 || ' minutes')::interval
  RETURNING id, driver_id, passenger_id
```

Each cancelled row fires `ride_cancelled` WS with `cancelled_by: "timeout"` to both parties.

---

## WebSocket events

| Type | Direction | Payload |
|---|---|---|
| `ride_request` | server → passenger | `{request_id, driver_id, driver_name, driver_avg_rating, driver_rating_count, car_make/model/color/plate, destination_lat/lng, corridor_km, seats}` |
| `ride_response` | server → driver | `{request_id, passenger_id, passenger_name, passenger_avg_rating, passenger_rating_count, status}` |
| `passenger_request` | server → driver | `{request_id, passenger_id, passenger_name, ..., pickup_lat, pickup_lng}` |
| `ride_done` | server → passenger | `{request_id, driver_id}` |
| `ride_cancelled` | server → either | `{request_id, cancelled_by: "driver"\|"passenger"\|"passenger_matched_elsewhere"\|"timeout"}` |
| `chat_message` | server → either | `{sender_id, content, created_at}` |

---

## Database schema (auto-migrated)

```sql
users (id, name, email UNIQUE, password (bcrypt), role,
       fcm_token, car_make, car_model, car_color, car_plate, created_at)

active_drivers (id, driver_id UNIQUE FK,
                origin/destination GEOGRAPHY(POINT,4326),
                route GEOGRAPHY(LINESTRING,4326),
                corridor_km, seats, seats_taken, status, created_at)

active_passengers (id, passenger_id UNIQUE FK,
                   location GEOGRAPHY(POINT,4326), updated_at)

ride_requests (id, driver_id FK, passenger_id FK,
               pickup_location GEOGRAPHY(POINT,4326), status, created_at,
               UNIQUE(driver_id, passenger_id))

completed_trips (id, ride_request_id, driver_id, passenger_id,
                 distance_km, co2_saved_kg, completed_at)

messages (id, ride_request_id FK, sender_id FK, content, created_at)

planned_trips (id, driver_id FK, origin_name, origin GEOGRAPHY,
               destination_name, destination GEOGRAPHY,
               route GEOGRAPHY(LINESTRING), seats, corridor_km,
               distance_km, planned_at, status, created_at)

ratings (id, rater_id FK, rated_id FK, ride_request_id FK,
         stars (1-5), comment, created_at,
         UNIQUE(rater_id, ride_request_id))
```

GIST indexes on `active_drivers.route`, `active_passengers.location`, and the `planned_trips.origin/destination` so all spatial queries are index-accelerated.

---

## PostGIS conventions

- All geo columns: `GEOGRAPHY` type, SRID 4326 (WGS84 lat/lng).
- `ST_MakePoint(lng, lat)` — PostGIS takes X=longitude first.
- `ST_DWithin(geography, geography, meters)` — always meters on geography type (km × 1000).
- `ST_Y(col::geometry)` = lat, `ST_X(col::geometry)` = lng.
- Routes stored as WKT: `LINESTRING(lng lat, lng lat, ...)`.
- OSRM URL also takes `lng,lat` coordinate order.

---

## Key design decisions

- **pgx/v5, not database/sql** — better PostgreSQL support, required for `GEOGRAPHY` type and `pgx.ErrNoRows` semantics inside transactions.
- **No ORM** — raw SQL throughout; PostGIS functions don't map well to ORMs.
- **GEOGRAPHY vs GEOMETRY** — GEOGRAPHY everywhere; `ST_DWithin` uses meters, accurate globally.
- **One active route per driver** — `UNIQUE(driver_id)` on `active_drivers` + upsert.
- **Passenger staleness filter** — `FindPassengersInCorridor` excludes `updated_at < NOW() - 10min`.
- **Firebase FCM is optional** — `notify.go` guards `if fcmClient == nil`; dev works without credentials.
- **No Redis** — driver positions live in PostGIS (`active_drivers.current_location`); the former Redis mirror was write-only and has been removed. Note the WS hub and the in-process throttles assume a single backend instance; scaling out requires moving that state to a shared store.
- **WSHub is a package-level singleton** in `handlers/websocket.go`; shared by all handlers.
- **Idempotency keys** — clients may send `X-Idempotency-Key` headers on mutating requests; currently passed through unchanged (room for server-side dedupe table later).

---

## Deploying to a server (Docker + HTTPS)

The deployable stack lives in `docker-compose.prod.yml`: PostGIS (internal
network only), the backend (built from `Dockerfile`), and Caddy terminating
TLS with automatic Let's Encrypt certificates. The mobile app must talk to a
**domain over HTTPS/WSS** — Android blocks plaintext HTTP and certificates
are not issued for bare IPs.

One-time setup:

1. **Domain** — create a free subdomain at [duckdns.org](https://www.duckdns.org)
   (e.g. `sameway.duckdns.org`) and set it to the server's public IP.
2. **Server** — any Linux VPS with Docker + the compose plugin installed and
   ports 80/443 open.
3. **Files** — copy this repository to the server, then next to
   `docker-compose.prod.yml` place:
   - `.env` — start from `.env.prod.example`; `DOMAIN`, `JWT_SECRET`
     (`openssl rand -base64 48`) and `DB_PASSWORD` are mandatory (compose
     refuses to boot without them);
   - `serviceAccountKey.json` — the Firebase service account (mounted
     read-only into the container, never baked into the image).

Run:

```bash
docker compose -f docker-compose.prod.yml up -d --build
curl https://<your-domain>/healthz   # → {"ok":true}
```

The Flutter app is pointed at the server at **build time**:

```bash
flutter build apk --release --dart-define=API_HOST=sameway.duckdns.org
```

(Without the define it falls back to the dev ngrok host.)

Notes:

- The stack runs `APP_ENV=dev` because routing still uses the public OSRM demo
  server, which the prod config gate deliberately rejects. The demo server is
  rate-limited — self-host OSRM and flip to `APP_ENV=prod` before real load.
- `GET /healthz` (unauthenticated) reports DB reachability; the compose
  healthcheck and any orchestrator probe use it.
- Postgres publishes **no ports**; it is reachable only from the compose
  network.

---

## Build status

`go build ./...` clean. `go vet ./...` clean. `go test ./...` clean.

## Running tests

The test suite uses [testcontainers-go](https://golang.testcontainers.org/) to
spin up a real Postgres+PostGIS container per test package. **Docker must be
running locally** (or in CI). No mocks for SQL — the bugs we ship are
SQL-level, so the tests run against actual PostGIS.

```bash
# Full suite (~30-60 s after first image pull; ~150 MB postgis image).
go test ./...

# Single package with verbose output:
go test ./services/ -v

# A specific regression test, e.g. the recent ON CONFLICT fix:
go test ./services/ -run TestCreateRideRequest_IdempotentOnExistingPending -v

# Coverage:
go test -coverprofile=cover.out ./...
go tool cover -func=cover.out | tail -1

# Race detector (slower, catches data races in the WS Hub):
go test -race ./...
```

### Test layout

- `services/*_test.go` — pure logic + SQL: `CreateRideRequest` idempotency,
  `FindPassengersInCorridor` heading filter, `GetNearbyDrivers` self-exclusion,
  OSRM client.
- `handlers/*_test.go` — every route in `main.go` with happy + failure cases.
- `middleware/auth_test.go` — JWT issuance and role enforcement.
- `db/postgres_test.go` — migration idempotency + schema invariants.
- `tests/e2e/flow_test.go` — cross-handler choreography: driver-initiated
  match, passenger-initiated match, role switch, self-pair guards, ride
  completion.

The Postgres+PostGIS image (`postgis/postgis:15-3.4-alpine`) is pulled once
and cached; subsequent runs are instant.
