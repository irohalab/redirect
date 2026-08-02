package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo"
)

func TestRedirectUsesRouteTokenAndRemovesItFromLocation(t *testing.T) {
	groups := newTestGroupManager()
	routing := newTestRoutingService(t)
	application := newApplication(groups, routing)

	resource := "/video/bangumi/episode.mp4?quality=source"
	token, _, err := routing.issueToken("secondary", resource)
	if err != nil {
		t.Fatalf("issue route token: %v", err)
	}
	playbackURL, err := routing.addToken(resource, token)
	if err != nil {
		t.Fatalf("create playback URL: %v", err)
	}

	e := echo.New()
	registerRoutes(e, application)
	request := httptest.NewRequest(http.MethodGet, playbackURL, nil)
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want %d; body: %s", recorder.Code, http.StatusTemporaryRedirect, recorder.Body.String())
	}
	location := recorder.Header().Get(echo.HeaderLocation)
	if location != "https://secondary.example/video/bangumi/episode.mp4?quality=source" {
		t.Fatalf("Location = %q", location)
	}
	if strings.Contains(location, routing.config.QueryParameter) {
		t.Fatalf("route token leaked to backend: %s", location)
	}
	if got := recorder.Header().Get("X-Mira-Backend"); got != "secondary" {
		t.Fatalf("X-Mira-Backend = %q, want secondary", got)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("Cache-Control = %q, want private, no-store", got)
	}
}

func TestRouteAPISelectsPreferredBackendWithoutExposingURL(t *testing.T) {
	groups := newTestGroupManager()
	routing := newTestRoutingService(t)
	application := newApplication(groups, routing)
	e := echo.New()
	registerRoutes(e, application)

	body := `{
		"resource": "/video/bangumi/episode.mp4?quality=source",
		"preference": {"mode": "backend", "backendId": "secondary"},
		"excludeBackendIds": []
	}`
	request := httptest.NewRequest(http.MethodPost, "/_mira/routing/v1/routes", strings.NewReader(body))
	request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body: %s", recorder.Code, http.StatusCreated, recorder.Body.String())
	}
	var response routeResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode route response: %v", err)
	}
	if response.SelectedBackend.ID != "secondary" {
		t.Fatalf("selected backend = %q, want secondary", response.SelectedBackend.ID)
	}
	if !strings.Contains(response.PlaybackURL, "__mira_route=") {
		t.Fatalf("playback URL has no route token: %s", response.PlaybackURL)
	}
	if !strings.HasPrefix(response.PlaybackURL, "https://media.example/video/") {
		t.Fatalf("playback URL is not absolute: %s", response.PlaybackURL)
	}
	if strings.Contains(recorder.Body.String(), "secondary.example") {
		t.Fatalf("route response exposed backend URL: %s", recorder.Body.String())
	}

	backendRequest := httptest.NewRequest(http.MethodGet, "/_mira/routing/v1/backends", nil)
	backendRecorder := httptest.NewRecorder()
	e.ServeHTTP(backendRecorder, backendRequest)
	if backendRecorder.Code != http.StatusOK {
		t.Fatalf("backend status = %d, want %d", backendRecorder.Code, http.StatusOK)
	}
	if strings.Contains(backendRecorder.Body.String(), "primary.example") || strings.Contains(backendRecorder.Body.String(), "secondary.example") {
		t.Fatalf("backend response exposed a URL: %s", backendRecorder.Body.String())
	}
}

func TestLegacyRedirectWithoutRoutingRemainsCompatible(t *testing.T) {
	disabledRouting, err := newRoutingServiceWithKeys(routingConfig{}, nil)
	if err != nil {
		t.Fatalf("create disabled routing service: %v", err)
	}
	application := newApplication(newTestGroupManager(), disabledRouting)
	e := echo.New()
	registerRoutes(e, application)

	request := httptest.NewRequest(http.MethodGet, "/video/bangumi/episode.mp4?quality=source&__mira_route=legacy-value", nil)
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusTemporaryRedirect)
	}
	if got := recorder.Header().Get(echo.HeaderLocation); got != "https://primary.example/video/bangumi/episode.mp4?quality=source&__mira_route=legacy-value" {
		t.Fatalf("Location = %q", got)
	}

	statusRequest := httptest.NewRequest(http.MethodPost, "/legacy/status", nil)
	statusRecorder := httptest.NewRecorder()
	e.ServeHTTP(statusRecorder, statusRequest)
	if statusRecorder.Code != http.StatusOK {
		t.Fatalf("legacy status = %d, want %d", statusRecorder.Code, http.StatusOK)
	}

	cookieRequest := httptest.NewRequest(http.MethodPut, "/secondary", nil)
	cookieRecorder := httptest.NewRecorder()
	e.ServeHTTP(cookieRecorder, cookieRequest)
	if cookieRecorder.Code != http.StatusOK {
		t.Fatalf("legacy cookie status = %d, want %d", cookieRecorder.Code, http.StatusOK)
	}
}

func TestInvalidAndExpiredRouteTokensFallBackWithoutLeaking(t *testing.T) {
	testCases := []struct {
		name        string
		playbackURL func(*routingService) string
	}{
		{
			name: "tampered",
			playbackURL: func(routing *routingService) string {
				resource := "/video/bangumi/episode.mp4?quality=source"
				token, _, err := routing.issueToken("secondary", resource)
				if err != nil {
					t.Fatalf("issue token: %v", err)
				}
				playbackURL, err := routing.addToken(resource, token+"x")
				if err != nil {
					t.Fatalf("add token: %v", err)
				}
				return playbackURL
			},
		},
		{
			name: "expired",
			playbackURL: func(routing *routingService) string {
				resource := "/video/bangumi/episode.mp4?quality=source"
				token, _, err := routing.issueToken("secondary", resource)
				if err != nil {
					t.Fatalf("issue token: %v", err)
				}
				playbackURL, err := routing.addToken(resource, token)
				if err != nil {
					t.Fatalf("add token: %v", err)
				}
				routing.now = func() time.Time {
					return time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)
				}
				return playbackURL
			},
		},
		{
			name: "duplicate",
			playbackURL: func(routing *routingService) string {
				resource := "/video/bangumi/episode.mp4?quality=source"
				token, _, err := routing.issueToken("secondary", resource)
				if err != nil {
					t.Fatalf("issue token: %v", err)
				}
				playbackURL, err := routing.addToken(resource, token)
				if err != nil {
					t.Fatalf("add token: %v", err)
				}
				return playbackURL + "&__mira_route=duplicate"
			},
		},
		{
			name: "malformed non-routing query",
			playbackURL: func(routing *routingService) string {
				return "/video/bangumi/episode.mp4?__mira_route=invalid&quality=source;legacy=true"
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			routing := newTestRoutingService(t)
			application := newApplication(newTestGroupManager(), routing)
			e := echo.New()
			registerRoutes(e, application)

			request := httptest.NewRequest(http.MethodGet, testCase.playbackURL(routing), nil)
			recorder := httptest.NewRecorder()
			e.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusTemporaryRedirect {
				t.Fatalf("status = %d, want %d; body: %s", recorder.Code, http.StatusTemporaryRedirect, recorder.Body.String())
			}
			location := recorder.Header().Get(echo.HeaderLocation)
			if !strings.HasPrefix(location, "https://primary.example/video/bangumi/episode.mp4") {
				t.Fatalf("Location = %q, expected primary fallback", location)
			}
			if strings.Contains(location, routing.config.QueryParameter) {
				t.Fatalf("routing parameter leaked to fallback: %s", location)
			}
		})
	}
}

func TestRoutingAPICORSAndRateLimit(t *testing.T) {
	routing := newTestRoutingService(t)
	routing.config.AllowedOrigins = []string{"https://mira.example"}
	routing.config.RequestsPerMinute = 1
	application := newApplication(newTestGroupManager(), routing)
	e := echo.New()
	registerRoutes(e, application)

	preflight := httptest.NewRequest(http.MethodOptions, "https://redirect.example/_mira/routing/v1/routes", nil)
	preflight.Header.Set("Origin", "https://mira.example")
	preflight.Header.Set("Access-Control-Request-Method", http.MethodPost)
	preflightRecorder := httptest.NewRecorder()
	e.ServeHTTP(preflightRecorder, preflight)
	if preflightRecorder.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want %d", preflightRecorder.Code, http.StatusNoContent)
	}
	if got := preflightRecorder.Header().Get("Access-Control-Allow-Origin"); got != "https://mira.example" {
		t.Fatalf("Access-Control-Allow-Origin = %q", got)
	}

	forbidden := httptest.NewRequest(http.MethodGet, "https://redirect.example/_mira/routing/v1/backends", nil)
	forbidden.Header.Set("Origin", "https://untrusted.example")
	forbiddenRecorder := httptest.NewRecorder()
	e.ServeHTTP(forbiddenRecorder, forbidden)
	if forbiddenRecorder.Code != http.StatusForbidden {
		t.Fatalf("forbidden origin status = %d, want %d", forbiddenRecorder.Code, http.StatusForbidden)
	}

	first := httptest.NewRequest(http.MethodGet, "/_mira/routing/v1/backends", nil)
	first.RemoteAddr = "192.0.2.10:1234"
	firstRecorder := httptest.NewRecorder()
	e.ServeHTTP(firstRecorder, first)
	if firstRecorder.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want %d", firstRecorder.Code, http.StatusOK)
	}

	second := httptest.NewRequest(http.MethodGet, "/_mira/routing/v1/backends", nil)
	second.RemoteAddr = "192.0.2.10:5678"
	secondRecorder := httptest.NewRecorder()
	e.ServeHTTP(secondRecorder, second)
	if secondRecorder.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d, want %d", secondRecorder.Code, http.StatusTooManyRequests)
	}
}

func TestRoutingConfigurationValidatesOrigins(t *testing.T) {
	_, err := newRoutingServiceWithKeys(routingConfig{
		Enabled:                 true,
		ActiveSigningKeyID:      "test-key",
		PublicOrigin:            "https://redirect.example/media",
		AllowedResourcePrefixes: []string{"/video/"},
	}, map[string][]byte{
		"test-key": bytes.Repeat([]byte{0x42}, 32),
	})
	if err == nil {
		t.Fatal("routing service accepted a public origin with a path")
	}
}

func newTestRoutingService(t *testing.T) *routingService {
	t.Helper()
	routing, err := newRoutingServiceWithKeys(routingConfig{
		Enabled:                 true,
		QueryParameter:          "__mira_route",
		FreshTTL:                "24h",
		MaxTTL:                  "168h",
		ActiveSigningKeyID:      "test-key",
		PublicOrigin:            "https://media.example",
		AllowedResourcePrefixes: []string{"/video/", "/pic/"},
	}, map[string][]byte{
		"test-key": bytes.Repeat([]byte{0x42}, 32),
	})
	if err != nil {
		t.Fatalf("create routing service: %v", err)
	}
	routing.now = func() time.Time {
		return time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC)
	}
	return routing
}

func newTestGroupManager() *groupManager {
	primary := &server{
		Name:       "primary",
		URL:        "https://primary.example",
		Label:      "Primary cache",
		Region:     "US",
		Selectable: true,
	}
	secondary := &server{
		Name:       "secondary",
		URL:        "https://secondary.example",
		Label:      "Secondary cache",
		Region:     "JP",
		Selectable: true,
	}
	mainGroup := &group{
		Name: "main",
		Type: "fallback",
		Servers: map[string]float64{
			"primary":   2,
			"secondary": 1,
		},
		servers: map[string]balanceGroup{
			"primary":   primary,
			"secondary": secondary,
		},
		sortedWeight: []serverWithWeight{
			{Name: "primary", Weight: 2},
			{Name: "secondary", Weight: 1},
		},
	}
	return &groupManager{
		servers: map[string]balanceGroup{
			"primary":   primary,
			"secondary": secondary,
			"main":      mainGroup,
		},
	}
}
