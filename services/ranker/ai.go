package ranker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/Pavelbotsu/sameway_backend/config"
)

// AIRanker calls an OpenAI-compatible chat-completions endpoint (Groq,
// Together, OpenRouter, Gemini compat layer, vLLM, Ollama, …) and asks it to
// score candidates by likely match quality.
//
// Privacy posture: we send only relative features (walk_m, drive_m, rating,
// rating_count) and a stable per-call index. We never send lat/lng, real
// names, or user IDs. The index → UserID mapping stays server-side.
//
// Failure posture: any error (timeout, non-2xx, malformed JSON, missing
// fields) returns an error so the caller (notifyCorridorMatches) can fall
// back to the geometric ordering and tag the result as
// "geometric_fallback".
//
// To delete the AI subsystem: remove this file, remove the Init branch in
// ranker.go, remove the four AI* fields from config/config.go. No other
// touches needed.
type AIRanker struct {
	httpClient *http.Client
	url        string
	apiKey     string
	model      string
}

func NewAIRanker(cfg config.Config) *AIRanker {
	return &AIRanker{
		httpClient: &http.Client{Timeout: 2 * time.Second},
		url:        cfg.AIProviderURL,
		apiKey:     cfg.AIProviderAPIKey,
		model:      cfg.AIModelName,
	}
}

func (a *AIRanker) Name() string { return "ai-" + a.model }

// promptCandidate is what we serialize into the LLM prompt. No PII.
type promptCandidate struct {
	Index           int     `json:"index"`
	WalkM           float64 `json:"walk_m"`
	DriverToPickupM float64 `json:"driver_to_pickup_m"`
	AvgRating       float64 `json:"avg_rating"`
	RatingCount     int     `json:"rating_count"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model          string        `json:"model"`
	Messages       []chatMessage `json:"messages"`
	Temperature    float64       `json:"temperature"`
	ResponseFormat struct {
		Type string `json:"type"`
	} `json:"response_format"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

type rankingItem struct {
	Index  int     `json:"index"`
	Score  float64 `json:"score"`
	Reason string  `json:"reason"`
}

type rankingPayload struct {
	Rankings []rankingItem `json:"rankings"`
}

// BuildPrompt is exported for the unit test that asserts no PII leaks. It
// returns the system + user messages and the anonymized features sent. The
// user-facing prompt asks for strict JSON only.
func (a *AIRanker) BuildPrompt(cands []Candidate, rc RankContext) (string, string, []promptCandidate) {
	features := make([]promptCandidate, len(cands))
	for i, c := range cands {
		features[i] = promptCandidate{
			Index:           i,
			WalkM:           c.WalkM,
			DriverToPickupM: c.DriverToPickupM,
			AvgRating:       c.AvgRating,
			RatingCount:     c.RatingCount,
		}
	}
	system := "You rank ride-share candidates by likely match quality. " +
		"Higher score = better match. Consider rating quality, walk distance, " +
		"and detour cost together. Return strict JSON only."
	featuresJSON, _ := json.Marshal(features)
	user := fmt.Sprintf(
		`Context: corridor %.1f km, %d seat(s).
Candidates: %s
Return: {"rankings":[{"index":<int>,"score":<0..1>,"reason":"<short>"}]}`,
		rc.CorridorKm, rc.Seats, string(featuresJSON),
	)
	return system, user, features
}

func (a *AIRanker) Rank(ctx context.Context, cands []Candidate, rc RankContext) ([]Result, error) {
	if len(cands) == 0 {
		return nil, nil
	}
	system, user, _ := a.BuildPrompt(cands, rc)

	body := chatRequest{
		Model: a.model,
		Messages: []chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		Temperature: 0.0,
	}
	body.ResponseFormat.Type = "json_object"

	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.url, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if a.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+a.apiKey)
	}

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("ranker http %d", resp.StatusCode)
	}

	var parsed chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	if len(parsed.Choices) == 0 {
		return nil, fmt.Errorf("ranker empty choices")
	}

	var payload rankingPayload
	if err := json.Unmarshal([]byte(parsed.Choices[0].Message.Content), &payload); err != nil {
		return nil, err
	}
	if len(payload.Rankings) == 0 {
		return nil, fmt.Errorf("ranker empty rankings")
	}

	out := make([]Result, 0, len(payload.Rankings))
	seen := make(map[int]bool, len(payload.Rankings))
	for _, r := range payload.Rankings {
		if r.Index < 0 || r.Index >= len(cands) || seen[r.Index] {
			continue
		}
		seen[r.Index] = true
		out = append(out, Result{
			UserID: cands[r.Index].UserID,
			Score:  r.Score,
			Reason: r.Reason,
		})
	}
	for i, c := range cands {
		if !seen[i] {
			out = append(out, Result{UserID: c.UserID, Score: 0})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out, nil
}
