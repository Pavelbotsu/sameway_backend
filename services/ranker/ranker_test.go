package ranker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pavelbotsu/sameway_backend/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGeometricRanker_PreservesOrder(t *testing.T) {
	cands := []Candidate{
		{UserID: "a", WalkM: 100, DriverToPickupM: 200},
		{UserID: "b", WalkM: 500, DriverToPickupM: 600},
		{UserID: "c", WalkM: 50, DriverToPickupM: 1000},
	}
	r, err := GeometricRanker{}.Rank(context.Background(), cands, RankContext{})
	require.NoError(t, err)
	require.Len(t, r, 3)
	assert.Equal(t, "a", r[0].UserID)
	assert.Equal(t, "b", r[1].UserID)
	assert.Equal(t, "c", r[2].UserID)
	// Scores are strictly decreasing so a stable sort by Score desc returns
	// the same order as the input.
	for i := 1; i < len(r); i++ {
		assert.Greater(t, r[i-1].Score, r[i].Score)
	}
}

func TestGeometricRanker_Name(t *testing.T) {
	assert.Equal(t, "geometric", GeometricRanker{}.Name())
}

func TestInit_DefaultStaysGeometric(t *testing.T) {
	prev := Active
	defer func() { Active = prev }()
	Active = &GeometricRanker{}
	Init(config.Config{}) // AIRankerEnabled=false, URL=""
	assert.Equal(t, "geometric", Active.Name())
}

func TestInit_AISwapWhenEnabledAndConfigured(t *testing.T) {
	prev := Active
	defer func() { Active = prev }()
	Active = &GeometricRanker{}
	Init(config.Config{
		AIRankerEnabled: true,
		AIProviderURL:   "http://example.test/v1/chat/completions",
		AIModelName:     "test-model",
	})
	assert.True(t, strings.HasPrefix(Active.Name(), "ai-"))
}

func TestAIRanker_BuildPrompt_OmitsPII(t *testing.T) {
	a := &AIRanker{model: "test"}
	cands := []Candidate{
		{UserID: "uid-1", Name: "Alice", WalkM: 100, DriverToPickupM: 200, AvgRating: 4.7, RatingCount: 52},
		{UserID: "uid-2", Name: "Bob", WalkM: 500, DriverToPickupM: 600, AvgRating: 4.2, RatingCount: 8},
	}
	_, user, features := a.BuildPrompt(cands, RankContext{DriverID: "driver-secret", CorridorKm: 2.5, Seats: 3})

	// Neither prompt half may contain user IDs or names.
	for _, banned := range []string{"uid-1", "uid-2", "Alice", "Bob", "driver-secret"} {
		assert.NotContains(t, user, banned, "leaked PII: %s", banned)
	}
	// Features should be the anonymized ones only.
	require.Len(t, features, 2)
	assert.Equal(t, 100.0, features[0].WalkM)
	assert.Equal(t, 4.7, features[0].AvgRating)
	assert.Equal(t, 0, features[0].Index)
	assert.Equal(t, 1, features[1].Index)
}

func TestAIRanker_HappyPath_ReordersByScore(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		assert.Contains(t, string(body), "Candidates")
		resp := map[string]any{
			"choices": []map[string]any{{
				"message": map[string]any{
					"content": `{"rankings":[{"index":2,"score":0.9,"reason":"best rating"},{"index":0,"score":0.5,"reason":"middling"},{"index":1,"score":0.1,"reason":"long detour"}]}`,
				},
			}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	a := NewAIRanker(config.Config{
		AIProviderURL: srv.URL,
		AIModelName:   "test",
	})
	cands := []Candidate{
		{UserID: "a"}, {UserID: "b"}, {UserID: "c"},
	}
	out, err := a.Rank(context.Background(), cands, RankContext{})
	require.NoError(t, err)
	require.Len(t, out, 3)
	assert.Equal(t, "c", out[0].UserID, "highest score first")
	assert.Equal(t, "a", out[1].UserID)
	assert.Equal(t, "b", out[2].UserID)
}

func TestAIRanker_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Second)
	}))
	defer srv.Close()

	a := NewAIRanker(config.Config{AIProviderURL: srv.URL, AIModelName: "slow"})
	// Tighten timeout on a copy for the test so we don't actually wait 2s.
	a.httpClient.Timeout = 100 * time.Millisecond

	start := time.Now()
	_, err := a.Rank(context.Background(), []Candidate{{UserID: "a"}, {UserID: "b"}}, RankContext{})
	assert.Error(t, err)
	assert.Less(t, time.Since(start), time.Second, "timeout must fire well under 1s")
}

func TestAIRanker_BadStatus_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	a := NewAIRanker(config.Config{AIProviderURL: srv.URL, AIModelName: "x"})
	_, err := a.Rank(context.Background(), []Candidate{{UserID: "a"}, {UserID: "b"}}, RankContext{})
	assert.Error(t, err)
}

func TestAIRanker_MalformedJSON_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"not json"}}]}`))
	}))
	defer srv.Close()
	a := NewAIRanker(config.Config{AIProviderURL: srv.URL, AIModelName: "x"})
	_, err := a.Rank(context.Background(), []Candidate{{UserID: "a"}, {UserID: "b"}}, RankContext{})
	assert.Error(t, err)
}

func TestAIRanker_AppendsMissingCandidates(t *testing.T) {
	// LLM returns rankings for only 2 of 3 candidates. The third should
	// still appear in the output (with zero score) so we never drop a
	// caller's input.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := `{"choices":[{"message":{"content":"{\"rankings\":[{\"index\":0,\"score\":0.8},{\"index\":1,\"score\":0.6}]}"}}]}`
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resp))
	}))
	defer srv.Close()
	a := NewAIRanker(config.Config{AIProviderURL: srv.URL, AIModelName: "x"})
	out, err := a.Rank(context.Background(), []Candidate{{UserID: "a"}, {UserID: "b"}, {UserID: "c"}}, RankContext{})
	require.NoError(t, err)
	require.Len(t, out, 3)
	assert.Equal(t, "a", out[0].UserID)
	assert.Equal(t, "b", out[1].UserID)
	assert.Equal(t, "c", out[2].UserID, "missing candidate appended at the end")
}
