package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	defaultRouteQueryParameter = "__mira_route"
	defaultRouteFreshTTL       = 24 * time.Hour
	defaultRouteMaxTTL         = 7 * 24 * time.Hour
	maxRouteTokenLength        = 4096
	maxRouteResourceLength     = 8192
)

var (
	errInvalidRouteToken     = errors.New("invalid route token")
	errExpiredRouteToken     = errors.New("expired route token")
	errRouteResourceMismatch = errors.New("route token does not match resource")
)

type routingConfig struct {
	Enabled                 bool
	QueryParameter          string
	FreshTTL                string
	MaxTTL                  string
	ActiveSigningKeyID      string
	SigningKeyEnvironments  map[string]string
	PublicOrigin            string
	AllowedOrigins          []string
	AllowedResourcePrefixes []string
	RequestsPerMinute       int
}

type routeClaims struct {
	Version      int    `json:"v"`
	KeyID        string `json:"kid"`
	RouteID      string `json:"jti"`
	Backend      string `json:"backend"`
	ResourceHash string `json:"resourceHash"`
	IssuedAt     int64  `json:"iat"`
	FreshUntil   int64  `json:"freshUntil"`
	ExpiresAt    int64  `json:"exp"`
}

type routingService struct {
	config   routingConfig
	keys     map[string][]byte
	freshTTL time.Duration
	maxTTL   time.Duration
	now      func() time.Time
}

func newRoutingService(config routingConfig) (*routingService, error) {
	keys := make(map[string][]byte)
	if config.Enabled {
		for keyID, environmentName := range config.SigningKeyEnvironments {
			encodedKey, ok := os.LookupEnv(environmentName)
			if !ok || strings.TrimSpace(encodedKey) == "" {
				return nil, fmt.Errorf("routing signing key environment %q is not set", environmentName)
			}
			key, err := decodeSigningKey(strings.TrimSpace(encodedKey))
			if err != nil {
				return nil, fmt.Errorf("decode routing signing key %q: %w", keyID, err)
			}
			keys[keyID] = key
		}
	}
	return newRoutingServiceWithKeys(config, keys)
}

func newRoutingServiceWithKeys(config routingConfig, keys map[string][]byte) (*routingService, error) {
	if config.QueryParameter == "" {
		config.QueryParameter = defaultRouteQueryParameter
	}
	if !isValidRouteQueryParameter(config.QueryParameter) {
		return nil, errors.New("routing query parameter must contain only RFC 3986 unreserved characters")
	}
	if config.PublicOrigin != "" {
		publicOrigin, err := normalizeHTTPOrigin(config.PublicOrigin)
		if err != nil {
			return nil, fmt.Errorf("parse routing PublicOrigin: %w", err)
		}
		config.PublicOrigin = publicOrigin
	}
	for index, allowedOrigin := range config.AllowedOrigins {
		normalizedOrigin, err := normalizeHTTPOrigin(allowedOrigin)
		if err != nil {
			return nil, fmt.Errorf("parse routing AllowedOrigins entry %q: %w", allowedOrigin, err)
		}
		config.AllowedOrigins[index] = normalizedOrigin
	}
	if len(config.AllowedResourcePrefixes) == 0 {
		config.AllowedResourcePrefixes = []string{"/video/", "/pic/"}
	}
	for _, prefix := range config.AllowedResourcePrefixes {
		if !strings.HasPrefix(prefix, "/") {
			return nil, fmt.Errorf("routing resource prefix %q must start with /", prefix)
		}
	}

	freshTTL, err := parseRouteDuration(config.FreshTTL, defaultRouteFreshTTL)
	if err != nil {
		return nil, fmt.Errorf("parse routing FreshTTL: %w", err)
	}
	maxTTL, err := parseRouteDuration(config.MaxTTL, defaultRouteMaxTTL)
	if err != nil {
		return nil, fmt.Errorf("parse routing MaxTTL: %w", err)
	}
	if freshTTL > maxTTL {
		return nil, errors.New("routing FreshTTL must not exceed MaxTTL")
	}

	keyCopy := make(map[string][]byte, len(keys))
	for keyID, key := range keys {
		if strings.TrimSpace(keyID) == "" {
			return nil, errors.New("routing signing key ID must not be empty")
		}
		if len(key) < 32 {
			return nil, fmt.Errorf("routing signing key %q must contain at least 32 bytes", keyID)
		}
		keyCopy[keyID] = append([]byte(nil), key...)
	}
	if config.Enabled {
		if config.ActiveSigningKeyID == "" {
			return nil, errors.New("routing ActiveSigningKeyID is required")
		}
		if _, ok := keyCopy[config.ActiveSigningKeyID]; !ok {
			return nil, fmt.Errorf("active routing signing key %q is not configured", config.ActiveSigningKeyID)
		}
	}

	return &routingService{
		config:   config,
		keys:     keyCopy,
		freshTTL: freshTTL,
		maxTTL:   maxTTL,
		now:      time.Now,
	}, nil
}

func isValidRouteQueryParameter(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			strings.ContainsRune("-._~", character) {
			continue
		}
		return false
	}
	return true
}

func normalizeHTTPOrigin(value string) (string, error) {
	parsed, err := url.Parse(value)
	if err != nil {
		return "", err
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" ||
		(parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return "", errors.New("origin must contain only an http or https scheme and host")
	}
	return strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host), nil
}

func parseRouteDuration(value string, defaultValue time.Duration) (time.Duration, error) {
	if value == "" {
		return defaultValue, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, err
	}
	if duration <= 0 {
		return 0, errors.New("duration must be positive")
	}
	return duration, nil
}

func decodeSigningKey(value string) ([]byte, error) {
	encodings := []*base64.Encoding{
		base64.RawStdEncoding,
		base64.StdEncoding,
		base64.RawURLEncoding,
		base64.URLEncoding,
	}
	for _, encoding := range encodings {
		if decoded, err := encoding.DecodeString(value); err == nil {
			return decoded, nil
		}
	}
	return nil, errors.New("signing key must be base64 encoded")
}

func (s *routingService) normalizeResource(rawResource string) (string, error) {
	resource, _, err := s.parseResource(rawResource, true)
	return resource, err
}

func (s *routingService) extractToken(rawResource string) (string, string, error) {
	return s.parseResource(rawResource, false)
}

func (s *routingService) parseResource(rawResource string, enforcePrefix bool) (string, string, error) {
	if len(rawResource) == 0 || len(rawResource) > maxRouteResourceLength {
		return "", "", errors.New("routing resource has invalid length")
	}
	parsed, err := url.ParseRequestURI(rawResource)
	if err != nil {
		return "", "", fmt.Errorf("parse routing resource: %w", err)
	}
	if parsed.IsAbs() || parsed.Host != "" || !strings.HasPrefix(parsed.Path, "/") {
		return "", "", errors.New("routing resource must be an absolute path on this origin")
	}
	if enforcePrefix && !s.isAllowedResourcePath(parsed.Path) {
		return "", "", errors.New("routing resource path is not allowed")
	}

	cleanQuery, tokens, hasToken, tokenErr := removeRouteQueryParameter(parsed.RawQuery, s.config.QueryParameter)
	resource := parsed.EscapedPath()
	if cleanQuery != "" {
		resource += "?" + cleanQuery
	}
	if !hasToken {
		return resource, "", nil
	}
	if tokenErr != nil || len(tokens) != 1 || tokens[0] == "" {
		return resource, "", errInvalidRouteToken
	}
	return resource, tokens[0], nil
}

func removeRouteQueryParameter(rawQuery string, parameterName string) (string, []string, bool, error) {
	if rawQuery == "" {
		return "", nil, false, nil
	}
	parts := strings.Split(rawQuery, "&")
	kept := make([]string, 0, len(parts))
	values := make([]string, 0, 1)
	found := false
	var routeValueError error
	for _, part := range parts {
		keyPart := part
		valuePart := ""
		if separator := strings.IndexByte(part, '='); separator >= 0 {
			keyPart = part[:separator]
			valuePart = part[separator+1:]
		}
		key, err := url.QueryUnescape(keyPart)
		if err != nil || key != parameterName {
			kept = append(kept, part)
			continue
		}
		found = true
		value, err := url.QueryUnescape(valuePart)
		if err != nil {
			routeValueError = errInvalidRouteToken
			continue
		}
		values = append(values, value)
	}
	return strings.Join(kept, "&"), values, found, routeValueError
}

func (s *routingService) isAllowedResourcePath(path string) bool {
	for _, prefix := range s.config.AllowedResourcePrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

func (s *routingService) addToken(resource string, token string) (string, error) {
	if token == "" {
		return "", errInvalidRouteToken
	}
	normalized, err := s.normalizeResource(resource)
	if err != nil {
		return "", err
	}
	separator := "?"
	if strings.Contains(normalized, "?") {
		separator = "&"
	}
	return normalized + separator + url.QueryEscape(s.config.QueryParameter) + "=" + url.QueryEscape(token), nil
}

func (s *routingService) issueToken(backend string, resource string) (string, routeClaims, error) {
	if !s.config.Enabled {
		return "", routeClaims{}, errors.New("routing is disabled")
	}
	if strings.TrimSpace(backend) == "" {
		return "", routeClaims{}, errors.New("routing backend is required")
	}
	normalized, err := s.normalizeResource(resource)
	if err != nil {
		return "", routeClaims{}, err
	}
	routeID, err := newRouteID()
	if err != nil {
		return "", routeClaims{}, fmt.Errorf("generate route ID: %w", err)
	}
	now := s.now().UTC()
	claims := routeClaims{
		Version:      1,
		KeyID:        s.config.ActiveSigningKeyID,
		RouteID:      routeID,
		Backend:      backend,
		ResourceHash: hashRouteResource(normalized),
		IssuedAt:     now.Unix(),
		FreshUntil:   now.Add(s.freshTTL).Unix(),
		ExpiresAt:    now.Add(s.maxTTL).Unix(),
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", routeClaims{}, err
	}
	payloadPart := base64.RawURLEncoding.EncodeToString(payload)
	signature := signRoutePayload([]byte(payloadPart), s.keys[claims.KeyID])
	token := payloadPart + "." + base64.RawURLEncoding.EncodeToString(signature)
	return token, claims, nil
}

func (s *routingService) verifyToken(token string, resource string) (routeClaims, error) {
	if len(token) == 0 || len(token) > maxRouteTokenLength {
		return routeClaims{}, errInvalidRouteToken
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return routeClaims{}, errInvalidRouteToken
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || base64.RawURLEncoding.EncodeToString(payload) != parts[0] {
		return routeClaims{}, errInvalidRouteToken
	}
	var claims routeClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return routeClaims{}, errInvalidRouteToken
	}
	key, ok := s.keys[claims.KeyID]
	if !ok {
		return routeClaims{}, errInvalidRouteToken
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || base64.RawURLEncoding.EncodeToString(signature) != parts[1] {
		return routeClaims{}, errInvalidRouteToken
	}
	expected := signRoutePayload([]byte(parts[0]), key)
	if !hmac.Equal(signature, expected) {
		return routeClaims{}, errInvalidRouteToken
	}
	if claims.Version != 1 || claims.RouteID == "" || claims.Backend == "" || claims.ResourceHash == "" {
		return routeClaims{}, errInvalidRouteToken
	}
	now := s.now().UTC().Unix()
	if claims.IssuedAt > now+60 || claims.FreshUntil < claims.IssuedAt || claims.ExpiresAt < claims.FreshUntil {
		return routeClaims{}, errInvalidRouteToken
	}
	if now >= claims.ExpiresAt {
		return routeClaims{}, errExpiredRouteToken
	}
	normalized, err := s.normalizeResource(resource)
	if err != nil {
		return routeClaims{}, err
	}
	if claims.ResourceHash != hashRouteResource(normalized) {
		return routeClaims{}, errRouteResourceMismatch
	}
	return claims, nil
}

func signRoutePayload(payload []byte, key []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(payload)
	return mac.Sum(nil)
}

func hashRouteResource(resource string) string {
	hash := sha256.Sum256([]byte(resource))
	return hex.EncodeToString(hash[:])
}

func newRouteID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}
