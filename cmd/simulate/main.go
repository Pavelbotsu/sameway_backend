// Simulation: 10 drivers + 50 passengers.
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
	latCenter = 50.08811 // Krakow center
	lngCenter = 20.02456
	spread    = 0.02 // ~5 km
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

var (
	wsOffersReceived int64
	wsAccepted       int64
	wsErrors         int64
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
		if ev["type"] == "ride_request" {
			atomic.AddInt64(&wsOffersReceived, 1)
			// Auto-accept the ride.
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
		}
	}
}

// ── driver WebSocket listener ─────────────────────────────────────────────────

var driverResponses int64

func listenDriver(u user, wg *sync.WaitGroup) {
	defer wg.Done()
	if u.Token == "" {
		return
	}
	url := wsBase + "/ws?token=" + u.Token
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
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
		if ev["type"] == "ride_response" {
			atomic.AddInt64(&driverResponses, 1)
		}
	}
}

// ── main ─────────────────────────────────────────────────────────────────────

func main() {
	start := time.Now()
	fmt.Println("═══════════════════════════════════════")
	fmt.Println("  sameway simulation: 100 drivers / 50 passengers")
	fmt.Println("═══════════════════════════════════════")

	// 1. Register all users.
	fmt.Println("\n[1/5] Registering users...")
	drivers := registerAll("sim_driver", "driver", 100)
	passengers := registerAll("sim_pass", "passenger", 50)
	dOK := countOK(drivers)
	pOK := countOK(passengers)
	fmt.Printf("  ✓ %d/100 drivers  |  %d/50 passengers registered\n", dOK, pOK)

	// 2. Connect all WebSocket listeners.
	fmt.Println("\n[2/5] Connecting WebSockets...")
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

	// 3. Update all passenger locations.
	fmt.Println("\n[3/5] Sending passenger locations...")
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
				"latitude":  randLat(),
				"longitude": randLng(),
			})
			if err == nil {
				atomic.AddInt64(&locOK, 1)
			}
		}(p)
	}
	locWg.Wait()
	fmt.Printf("  ✓ %d/%d location updates sent\n", locOK, pOK)

	// 4. Set driver routes (corridor covers the same area as passengers).
	fmt.Println("\n[4/5] Setting driver routes...")
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
	fmt.Printf("  ✓ %d/10 routes set  |  %d total passenger notifications sent\n",
		routeOK, totalNotified)

	// 5. Wait for WS events to propagate.
	fmt.Println("\n[5/5] Waiting for WebSocket events (10s)...")
	time.Sleep(10 * time.Second)

	// 6. Check driver request counts.
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
	fmt.Printf("  Users registered       %d drivers / %d passengers\n", dOK, pOK)
	fmt.Printf("  Location updates sent  %d / %d\n", locOK, pOK)
	fmt.Printf("  Driver routes set      %d / 10\n", routeOK)
	fmt.Printf("  Passengers notified    %d (via backend)\n", totalNotified)
	fmt.Printf("  WS ride_request events %d received by passengers\n", wsOffersReceived)
	fmt.Printf("  Passengers accepted    %d rides via WS\n", wsAccepted)
	fmt.Printf("  Driver ride_responses  %d received\n", driverResponses)
	fmt.Printf("  Pending requests in DB %d\n", totalRequests)
	fmt.Printf("  WS errors              %d\n", wsErrors)
	fmt.Printf("  Total time             %s\n", elapsed)
	fmt.Println("═══════════════════════════════════════")

	if wsErrors > 0 {
		fmt.Println("\n  ⚠  WS errors usually mean the backend WS endpoint is not running.")
	}
	if totalNotified == 0 {
		fmt.Println("\n  ⚠  0 passengers notified. Likely causes:")
		fmt.Println("     • OSRM not running (route calculation failed)")
		fmt.Println("     • Passengers not in PostGIS yet when routes were set")
		fmt.Println("     • DB connection issue")
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
