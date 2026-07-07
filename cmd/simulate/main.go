// Simulation: drivers + passengers exercising both connection flows.
// Run from sameway_backend/: go run ./cmd/simulate/
// Backend must be running on localhost:8080.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const base = "http://localhost:8080"
const wsBase = "ws://localhost:8080"

// City center + spread — change to your city.
const (
	//latCenter = 50.06465 // Krakow center
	//lngCenter = 19.94498
	latCenter = 49.091339 // Kremenchuk center
	lngCenter = 33.379375
	spread    = 0.02 // ~5 km
)

const (
	numDrivers    = 100
	numPassengers = 50
	// Fraction of passengers that, after seeing drivers via /nearby-drivers,
	// will pick one and POST /passenger/request-driver (passenger-initiated flow).
	requestDriverFraction = 0.5
)

// ── helpers ──────────────────────────────────────────────────────────────────

type user struct {
	Token  string `json:"token"`
	UserID string `json:"user_id"`
	Role   string `json:"role"`
	name   string
}

func randLat() float64 { return latCenter + (rand.Float64()*2-1)*spread }
func randLng() float64 { return lngCenter + (rand.Float64()*2-1)*spread }

func post(path, token string, body any) (map[string]any, error) {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, base+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	json.Unmarshal(raw, &m)
	if resp.StatusCode >= 400 {
		return m, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(raw))
	}
	return m, nil
}

// postList is the same as post but decodes a JSON list response (e.g.
// /passenger/nearby-drivers returns an array, not an object).
func postList(path, token string, body any) ([]map[string]any, error) {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, base+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(raw))
	}
	var list []map[string]any
	json.Unmarshal(raw, &list)
	return list, nil
}

func put(path, token string, body any) error {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPut, base+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func getJSON(path, token string) ([]map[string]any, error) {
	req, _ := http.NewRequest(http.MethodGet, base+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var list []map[string]any
	json.Unmarshal(raw, &list)
	return list, nil
}

// ── registration ─────────────────────────────────────────────────────────────

func register(name, role string) (user, error) {
	email := name + "@test2.com"
	m, err := post("/auth/register", "", map[string]any{
		"name":     name,
		"email":    email,
		"password": "simpass123",
		"role":     role,
	})
	if err != nil {
		// Fall back to login when user already exists (409).
		m, err = post("/auth/login", "", map[string]any{
			"email":    email,
			"password": "simpass123",
		})
		if err != nil {
			return user{}, err
		}
	}
	u := user{name: name}
	if t, ok := m["token"].(string); ok {
		u.Token = t
	}
	if id, ok := m["user_id"].(string); ok {
		u.UserID = id
	}
	u.Role = role
	if u.Token == "" {
		return u, fmt.Errorf("no token in response: %v", m)
	}
	return u, nil
}

func registerAll(prefix, role string, n int) []user {
	users := make([]user, n)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs int
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("%s%d", prefix, i)
			u, err := register(name, role)
			if err != nil {
				mu.Lock()
				errs++
				fmt.Printf("  ✗ %s: %v\n", name, err)
				mu.Unlock()
				return
			}
			users[i] = u
		}(i)
	}
	wg.Wait()
	if errs > 0 {
		fmt.Printf("  %d registration errors\n", errs)
	}
	return users
}

// ── passenger WebSocket listener ─────────────────────────────────────────────
//
// Auto-accepts every `ride_request` (driver-initiated flow) and tracks
// `driver_response` (passenger-initiated flow — confirms the driver accepted).

var (
	wsOffersReceived   int64 // ride_request events (driver-initiated)
	wsAccepted         int64 // passenger /respond accept calls
	driverResponseEvts int64 // driver_response received (passenger-initiated)
	wsErrors           int64
)

func listenPassenger(u user, wg *sync.WaitGroup) {
	defer wg.Done()
	if u.Token == "" {
		return
	}
	url := wsBase + "/ws?token=" + u.Token
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		atomic.AddInt64(&wsErrors, 1)
		return
	}
	defer conn.Close()

	// Listen for up to 30 seconds.
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var ev map[string]any
		if json.Unmarshal(msg, &ev) != nil {
			continue
		}
		switch ev["type"] {
		case "ride_request":
			atomic.AddInt64(&wsOffersReceived, 1)
			payload, _ := ev["payload"].(map[string]any)
			reqID, _ := payload["request_id"].(string)
			if reqID != "" {
				_, err := post("/passenger/respond", u.Token, map[string]any{
					"request_id": reqID,
					"status":     "accepted",
				})
				if err == nil {
					atomic.AddInt64(&wsAccepted, 1)
				}
			}
		case "driver_response":
			atomic.AddInt64(&driverResponseEvts, 1)
		}
	}
}

// ── driver WebSocket listener ─────────────────────────────────────────────────
//
// Auto-accepts every `passenger_request` (passenger-initiated flow) and tracks
// `ride_response` (driver-initiated flow — confirms the passenger accepted).

var (
	driverResponses        int64 // ride_response received (driver-initiated)
	passengerRequestEvts   int64 // passenger_request received (passenger-initiated)
	driverPassengerAccepts int64 // driver /respond accept calls
)

func listenDriver(u user, wg *sync.WaitGroup) {
	defer wg.Done()
	if u.Token == "" {
		return
	}
	url := wsBase + "/ws?token=" + u.Token
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		atomic.AddInt64(&wsErrors, 1)
		return
	}
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var ev map[string]any
		if json.Unmarshal(msg, &ev) != nil {
			continue
		}
		switch ev["type"] {
		case "ride_response":
			atomic.AddInt64(&driverResponses, 1)
		case "passenger_request":
			atomic.AddInt64(&passengerRequestEvts, 1)
			payload, _ := ev["payload"].(map[string]any)
			reqID, _ := payload["request_id"].(string)
			if reqID != "" {
				_, err := post("/driver/respond", u.Token, map[string]any{
					"request_id": reqID,
					"status":     "accepted",
				})
				if err == nil {
					atomic.AddInt64(&driverPassengerAccepts, 1)
				}
			}
		}
	}
}

// ── main ─────────────────────────────────────────────────────────────────────

func main() {
	start := time.Now()
	fmt.Println("═══════════════════════════════════════")
	fmt.Printf("  sameway simulation: %d drivers / %d passengers\n",
		numDrivers, numPassengers)
	fmt.Println("═══════════════════════════════════════")

	// 1. Register all users.
	fmt.Println("\n[1/7] Registering users...")
	drivers := registerAll("sim_driver", "driver", numDrivers)
	passengers := registerAll("sim_pass", "passenger", numPassengers)
	dOK := countOK(drivers)
	pOK := countOK(passengers)
	fmt.Printf("  ✓ %d/%d drivers  |  %d/%d passengers registered\n",
		dOK, numDrivers, pOK, numPassengers)

	// 2. Connect all WebSocket listeners.
	fmt.Println("\n[2/7] Connecting WebSockets...")
	var wsWg sync.WaitGroup
	for _, p := range passengers {
		if p.Token == "" {
			continue
		}
		wsWg.Add(1)
		go listenPassenger(p, &wsWg)
	}
	for _, d := range drivers {
		if d.Token == "" {
			continue
		}
		wsWg.Add(1)
		go listenDriver(d, &wsWg)
	}
	time.Sleep(500 * time.Millisecond) // let connections settle
	fmt.Printf("  ✓ WS connections opened\n")

	// 3. Update all passenger locations (required for both flows).
	fmt.Println("\n[3/7] Sending passenger locations...")
	var locWg sync.WaitGroup
	var locOK int64
	for _, p := range passengers {
		if p.Token == "" {
			continue
		}
		locWg.Add(1)
		go func(p user) {
			defer locWg.Done()
			err := put("/passenger/location", p.Token, map[string]any{
				"lat": randLat(),
				"lng": randLng(),
			})
			if err == nil {
				atomic.AddInt64(&locOK, 1)
			}
		}(p)
	}
	locWg.Wait()
	fmt.Printf("  ✓ %d/%d location updates sent\n", locOK, pOK)

	// 4. Set driver routes — triggers driver-initiated flow:
	//    backend matches passengers in the corridor and emits `ride_request` WS
	//    events; listenPassenger auto-accepts each.
	fmt.Println("\n[4/7] Setting driver routes (driver-initiated flow)...")
	var routeWg sync.WaitGroup
	var totalNotified int64
	var routeOK int64
	for _, d := range drivers {
		if d.Token == "" {
			continue
		}
		routeWg.Add(1)
		go func(d user) {
			defer routeWg.Done()
			result, err := post("/driver/route", d.Token, map[string]any{
				"origin_lat":      randLat(),
				"origin_lng":      randLng(),
				"destination_lat": latCenter + spread + rand.Float64()*0.02,
				"destination_lng": lngCenter + spread + rand.Float64()*0.02,
				"corridor_km":     6.0,
				"seats":           rand.Intn(3) + 1,
			})
			if err != nil {
				return
			}
			atomic.AddInt64(&routeOK, 1)
			if n, ok := result["notified_count"].(float64); ok {
				atomic.AddInt64(&totalNotified, int64(n))
			}
		}(d)
	}
	routeWg.Wait()
	fmt.Printf("  ✓ %d/%d routes set  |  %d total passenger notifications sent\n",
		routeOK, numDrivers, totalNotified)

	// 5. Let driver-initiated rides settle so seats reserved by the
	//    `ride_request` auto-accepts don't collide with the
	//    passenger-initiated burst in step 6.
	fmt.Println("\n[5/7] Waiting 4s for driver-initiated WS to settle...")
	time.Sleep(4 * time.Second)

	// 6. Passenger-initiated flow: half of passengers call /nearby-drivers,
	//    pick one at random, and POST /passenger/request-driver. The matched
	//    driver's WS listener auto-accepts via /driver/respond.
	fmt.Println("\n[6/7] Passenger-initiated requests...")
	var reqWg sync.WaitGroup
	var nearbyOK, requestSent int64
	wantRequesters := int(float64(pOK) * requestDriverFraction)
	requesters := pickN(passengers, wantRequesters)
	for _, p := range requesters {
		if p.Token == "" {
			continue
		}
		reqWg.Add(1)
		go func(p user) {
			defer reqWg.Done()
			nearby, err := postList("/passenger/nearby-drivers", p.Token,
				map[string]any{
					"latitude":  randLat(),
					"longitude": randLng(),
					"radius_km": 10.0,
				})
			if err != nil || len(nearby) == 0 {
				return
			}
			atomic.AddInt64(&nearbyOK, 1)
			driverID, _ := nearby[rand.Intn(len(nearby))]["driver_id"].(string)
			if driverID == "" {
				return
			}
			_, err = post("/passenger/request-driver", p.Token, map[string]any{
				"driver_id": driverID,
			})
			if err == nil {
				atomic.AddInt64(&requestSent, 1)
			}
		}(p)
	}
	reqWg.Wait()
	fmt.Printf("  ✓ %d/%d nearby-drivers calls  |  %d /request-driver POSTs\n",
		nearbyOK, len(requesters), requestSent)

	// 7. Wait for the passenger-initiated WS round-trip to complete.
	fmt.Println("\n[7/7] Waiting 6s for passenger-initiated WS to settle...")
	time.Sleep(6 * time.Second)

	// 8. Snapshot the per-driver request queue.
	var totalRequests int
	for _, d := range drivers {
		if d.Token == "" {
			continue
		}
		reqs, err := getJSON("/driver/requests", d.Token)
		if err == nil {
			totalRequests += len(reqs)
		}
	}

	// Print summary.
	elapsed := time.Since(start).Round(time.Millisecond)
	fmt.Println("\n═══════════════════════════════════════")
	fmt.Println("  RESULTS")
	fmt.Println("═══════════════════════════════════════")
	fmt.Printf("  Users registered           %d drivers / %d passengers\n",
		dOK, pOK)
	fmt.Printf("  Location updates sent      %d / %d\n", locOK, pOK)
	fmt.Printf("  Driver routes set          %d / %d\n", routeOK, numDrivers)
	fmt.Println("  -- Driver-initiated flow --")
	fmt.Printf("  Passengers notified        %d (via backend match)\n",
		totalNotified)
	fmt.Printf("  WS ride_request events     %d received by passengers\n",
		wsOffersReceived)
	fmt.Printf("  Passengers accepted        %d rides via WS auto-accept\n",
		wsAccepted)
	fmt.Printf("  Driver ride_response evts  %d received\n", driverResponses)
	fmt.Println("  -- Passenger-initiated flow --")
	fmt.Printf("  /nearby-drivers OK         %d / %d\n",
		nearbyOK, len(requesters))
	fmt.Printf("  /request-driver POSTs      %d sent\n", requestSent)
	fmt.Printf("  WS passenger_request evts  %d received by drivers\n",
		passengerRequestEvts)
	fmt.Printf("  Drivers accepted           %d rides via WS auto-accept\n",
		driverPassengerAccepts)
	fmt.Printf("  Passenger driver_response  %d received\n", driverResponseEvts)
	fmt.Println("  --")
	fmt.Printf("  Pending requests in DB     %d\n", totalRequests)
	fmt.Printf("  WS errors                  %d\n", wsErrors)
	fmt.Printf("  Total time                 %s\n", elapsed)
	fmt.Println("═══════════════════════════════════════")

	if wsErrors > 0 {
		fmt.Println("\n  ⚠  WS errors usually mean the backend WS endpoint is not running.")
	}
	if totalNotified == 0 {
		fmt.Println("\n  ⚠  0 passengers notified by driver-initiated flow. Likely causes:")
		fmt.Println("     • OSRM not running (route calculation failed)")
		fmt.Println("     • Passengers not in PostGIS yet when routes were set")
		fmt.Println("     • Corridor query filter excluded passengers with live requests")
	}
	if requestSent > 0 && passengerRequestEvts == 0 {
		fmt.Println("\n  ⚠  /request-driver posted but no passenger_request WS reached drivers.")
		fmt.Println("     • Check Hub.SendToUser / WS handler in handlers/passenger.go")
	}
}

func countOK(users []user) int {
	n := 0
	for _, u := range users {
		if u.Token != "" {
			n++
		}
	}
	return n
}

// pickN returns up to n distinct users with a non-empty token, in input order.
func pickN(users []user, n int) []user {
	out := make([]user, 0, n)
	for _, u := range users {
		if u.Token == "" {
			continue
		}
		out = append(out, u)
		if len(out) >= n {
			break
		}
	}
	return out
}
