package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestHealthCheckClientHasTimeout(t *testing.T) {
	if healthCheckClient.Timeout != healthCheckTimeout || healthCheckClient.Timeout <= 0 {
		t.Fatalf("health check timeout = %v, want %v", healthCheckClient.Timeout, healthCheckTimeout)
	}
}

func TestHealthCheckHonorsClientTimeout(t *testing.T) {
	requestStarted := make(chan struct{})
	client := &http.Client{
		Timeout: 20 * time.Millisecond,
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			close(requestStarted)
			<-request.Context().Done()
			return nil, request.Context().Err()
		}),
	}

	backend := &server{URL: "https://cache.example"}
	startedAt := time.Now()
	online, err := backend.checkHealth(client)
	if err == nil || online {
		t.Fatalf("checkHealth() = (%v, %v), want timeout failure", online, err)
	}
	if elapsed := time.Since(startedAt); elapsed >= time.Second {
		t.Fatalf("health check took %v, want less than one second", elapsed)
	}
	select {
	case <-requestStarted:
	default:
		t.Fatal("health check request did not reach upstream")
	}
}

func TestServerURLValidation(t *testing.T) {
	for _, value := range []string{
		"",
		"cache.example",
		"://invalid",
		"ftp://cache.example",
		"https://cache.example?token=value",
		"https://cache.example?",
		"https://cache.example#fragment",
		"https://cache.example#",
	} {
		if err := validateServerURL(value); err == nil {
			t.Errorf("validateServerURL(%q) succeeded", value)
		}
	}
	for _, value := range []string{"http://cache.example", "https://cache.example:8443", "https://cache.example/base/"} {
		if err := validateServerURL(value); err != nil {
			t.Errorf("validateServerURL(%q) failed: %v", value, err)
		}
	}
}

func TestHealthCheckRejectsInvalidURLWithoutPanicking(t *testing.T) {
	backend := &server{URL: "://invalid"}
	if online, err := backend.checkHealth(&http.Client{}); err == nil || online {
		t.Fatalf("checkHealth() = (%v, %v), want invalid URL failure", online, err)
	}
}

func TestUpdateHealthStatusTracksTransitions(t *testing.T) {
	backend := &server{Name: "backend"}
	backend.updateHealthStatus(false, errors.New("health check failed"))
	if !backend.Offline || !backend.LastOnline.IsZero() {
		t.Fatalf("offline state = (%v, %v)", backend.Offline, backend.LastOnline)
	}

	backend.updateHealthStatus(true, nil)
	if backend.Offline || backend.LastOnline.IsZero() {
		t.Fatalf("online state = (%v, %v)", backend.Offline, backend.LastOnline)
	}
}

func TestValidateGroupConfigurationsRejectsCycles(t *testing.T) {
	err := validateGroupConfigurations(nil, map[string]json.RawMessage{
		"a": json.RawMessage(`{"Servers":{"b":1}}`),
		"b": json.RawMessage(`{"Servers":{"a":1}}`),
	})
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle validation error = %v", err)
	}
}

func TestValidateGroupConfigurationsRejectsInvalidMembers(t *testing.T) {
	serverConfigs := map[string]json.RawMessage{"backend": json.RawMessage(`{}`)}
	testCases := []struct {
		name   string
		config string
	}{
		{name: "zero weight", config: `{"Servers":{"backend":0}}`},
		{name: "negative weight", config: `{"Servers":{"backend":-1}}`},
		{name: "unknown member", config: `{"Servers":{"missing":1}}`},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			err := validateGroupConfigurations(serverConfigs, map[string]json.RawMessage{
				"main": json.RawMessage(testCase.config),
			})
			if err == nil {
				t.Fatal("group configuration was accepted")
			}
		})
	}
}

func TestValidateGroupConfigurationsAcceptsNestedGroups(t *testing.T) {
	err := validateGroupConfigurations(
		map[string]json.RawMessage{"backend": json.RawMessage(`{}`)},
		map[string]json.RawMessage{
			"regional": json.RawMessage(`{"Servers":{"backend":1}}`),
			"main":     json.RawMessage(`{"Servers":{"regional":1}}`),
		},
	)
	if err != nil {
		t.Fatalf("valid nested groups were rejected: %v", err)
	}
}

func TestGroupCalculatesTotalWeight(t *testing.T) {
	configured := (&group{}).init("main", []byte(`{"Type":"random","Servers":{"primary":2.5,"secondary":1}}`))
	if configured.totalWeight != 3.5 {
		t.Fatalf("totalWeight = %v, want 3.5", configured.totalWeight)
	}
}
