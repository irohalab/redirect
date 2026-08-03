package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestRoutingTokenIsBoundToResource(t *testing.T) {
	now := time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC)
	routing, err := newRoutingServiceWithKeys(routingConfig{
		Enabled:                 true,
		QueryParameter:          "__mira_route",
		FreshTTL:                "24h",
		MaxTTL:                  "168h",
		ActiveSigningKeyID:      "test-key",
		AllowedResourcePrefixes: []string{"/video/", "/pic/"},
	}, map[string][]byte{
		"test-key": bytes.Repeat([]byte{0x42}, 32),
	})
	if err != nil {
		t.Fatalf("create routing service: %v", err)
	}
	routing.now = func() time.Time { return now }

	resource, err := routing.normalizeResource("/video/bangumi/episode.mp4?quality=source")
	if err != nil {
		t.Fatalf("normalize resource: %v", err)
	}
	token, claims, err := routing.issueToken("jp-oracle", resource)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	if claims.Backend != "jp-oracle" {
		t.Fatalf("backend = %q, want jp-oracle", claims.Backend)
	}

	playbackURL, err := routing.addToken(resource, token)
	if err != nil {
		t.Fatalf("add route token: %v", err)
	}
	if !strings.Contains(playbackURL, "quality=source") || !strings.Contains(playbackURL, "__mira_route=") {
		t.Fatalf("playback URL did not preserve query and add token: %s", playbackURL)
	}

	requestResource, requestToken, err := routing.extractToken(playbackURL)
	if err != nil {
		t.Fatalf("extract route token: %v", err)
	}
	if requestResource != resource {
		t.Fatalf("resource after token removal = %q, want %q", requestResource, resource)
	}
	verified, err := routing.verifyToken(requestToken, requestResource)
	if err != nil {
		t.Fatalf("verify token: %v", err)
	}
	if verified.Backend != "jp-oracle" {
		t.Fatalf("verified backend = %q, want jp-oracle", verified.Backend)
	}

	if _, err := routing.verifyToken(token, "/video/bangumi/other.mp4?quality=source"); err == nil {
		t.Fatal("token verified for a different resource")
	}

	replacement := "A"
	if strings.HasSuffix(token, replacement) {
		replacement = "B"
	}
	tampered := token[:len(token)-1] + replacement
	if _, err := routing.verifyToken(tampered, resource); err == nil {
		t.Fatal("tampered token verified")
	}

	routing.now = func() time.Time { return now.Add(169 * time.Hour) }
	if _, err := routing.verifyToken(token, resource); err == nil {
		t.Fatal("expired token verified")
	}
}

func TestRoutingResourceRejectsDotPathSegments(t *testing.T) {
	routing, err := newRoutingServiceWithKeys(routingConfig{
		AllowedResourcePrefixes: []string{"/video/", "/pic/"},
	}, nil)
	if err != nil {
		t.Fatalf("create routing service: %v", err)
	}

	testCases := []string{
		"/video/../private/file.mp4",
		"/video/./episode.mp4",
		"/video/%2e%2e/private/file.mp4",
		"/video/segment/%2E/episode.mp4",
		"/video%2f..%2fprivate/file.mp4",
	}
	for _, resource := range testCases {
		if _, err := routing.normalizeResource(resource); err == nil {
			t.Errorf("normalizeResource(%q) accepted a dot path segment", resource)
		}
	}

	if _, err := routing.normalizeResource("/video/bangumi/episode.mp4"); err != nil {
		t.Fatalf("valid routing resource was rejected: %v", err)
	}
}
