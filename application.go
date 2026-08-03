package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo"
)

const (
	defaultRouteRequestsPerMinute = 120
	maxRouteRequestBodySize       = 16 * 1024
	maxExcludedBackends           = 16
	maxRateLimitEntries           = 10000
)

type application struct {
	groups  *groupManager
	routing *routingService
	limiter *fixedWindowRateLimiter
}

type backendMetadata struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	Region       string `json:"region,omitempty"`
	Availability string `json:"availability"`
	Selectable   bool   `json:"selectable"`
}

type backendListResponse struct {
	Version  int               `json:"version"`
	Backends []backendMetadata `json:"backends"`
}

type routePreference struct {
	Mode      string `json:"mode"`
	BackendID string `json:"backendId,omitempty"`
}

type routeRequest struct {
	Resource          string          `json:"resource"`
	Preference        routePreference `json:"preference"`
	ExcludeBackendIDs []string        `json:"excludeBackendIds"`
}

type routeSelection struct {
	Mode   string `json:"mode"`
	Reason string `json:"reason"`
}

type routeResponse struct {
	RouteToken      string          `json:"routeToken"`
	PlaybackURL     string          `json:"playbackUrl"`
	SelectedBackend backendMetadata `json:"selectedBackend"`
	Selection       routeSelection  `json:"selection"`
	IssuedAt        time.Time       `json:"issuedAt"`
	FreshUntil      time.Time       `json:"freshUntil"`
	ExpiresAt       time.Time       `json:"expiresAt"`
}

type rateLimitWindow struct {
	startedAt time.Time
	requests  int
}

type fixedWindowRateLimiter struct {
	mutex       sync.Mutex
	limit       int
	window      time.Duration
	now         func() time.Time
	lastCleanup time.Time
	values      map[string]rateLimitWindow
}

func newApplication(groups *groupManager, routing *routingService) *application {
	if routing != nil && !routing.config.Enabled {
		routing = nil
	}
	requestsPerMinute := defaultRouteRequestsPerMinute
	if routing != nil && routing.config.RequestsPerMinute > 0 {
		requestsPerMinute = routing.config.RequestsPerMinute
	}
	return &application{
		groups:  groups,
		routing: routing,
		limiter: &fixedWindowRateLimiter{
			limit:  requestsPerMinute,
			window: time.Minute,
			now:    time.Now,
			values: make(map[string]rateLimitWindow),
		},
	}
}

func registerRoutes(e *echo.Echo, application *application) {
	if application.routing != nil && application.routing.config.Enabled {
		middleware := []echo.MiddlewareFunc{application.routingCORSMiddleware, application.routeRateLimitMiddleware}
		optionsHandler := func(c echo.Context) error {
			return c.NoContent(http.StatusNoContent)
		}
		e.OPTIONS("/_mira/routing/v1/backends", optionsHandler, middleware...)
		e.OPTIONS("/_mira/routing/v1/routes", optionsHandler, middleware...)
		e.GET("/_mira/routing/v1/backends", application.listBackends, middleware...)
		e.POST("/_mira/routing/v1/routes", application.createRoute, middleware...)
	}

	e.GET("/*", application.redirect)
	e.HEAD("/*", application.redirect)
	e.POST("/*", application.status)
	e.PUT("/:group", application.setCookie)
	e.DELETE("/", application.deleteCookie)
}

func (a *application) redirect(c echo.Context) error {
	resource := c.Request().URL.RequestURI()
	selectedRoute := "legacy"
	var selected *server

	if a.routing != nil {
		cleanResource, token, extractErr := a.routing.extractToken(resource)
		resource = cleanResource
		if token != "" || errors.Is(extractErr, errInvalidRouteToken) {
			selectedRoute = "fallback"
			if extractErr == nil {
				if claims, err := a.routing.verifyToken(token, resource); err == nil {
					if candidate := a.groups.getConcreteServer(claims.Backend); candidate != nil {
						selected = candidate.selectServer(serverSelectionOptions{
							count:          true,
							selectableOnly: true,
						})
						if selected != nil {
							selectedRoute = "pinned"
						}
					}
				}
			}
		}
	}

	if selected == nil {
		group := a.groups.getByGroup(c)
		if group == nil {
			group = a.groups.getByIP(c)
		}
		if group == nil {
			group = a.groups.get("main")
		}
		if group == nil {
			return errors.New("empty main group")
		}
		selected = group.getServer()
		if selected == nil {
			return errors.New("no server found")
		}
		if selectedRoute != "legacy" {
			selectedRoute = "fallback"
		}
	}

	if verboseMode {
		if cookie, err := c.Cookie("group"); err == nil {
			log.Printf("Request from [%s] to [%s] with cookie [%s] has been redirected to [%s]\n", c.RealIP(), resource, cookie.Value, selected.Name)
		} else {
			log.Printf("Request from [%s] to [%s] has been redirected to [%s]\n", c.RealIP(), resource, selected.Name)
		}
	}

	redirectType := selected.RedirectType
	if redirectType == 0 {
		redirectType = a.groups.RedirectType
	}
	if redirectType == 0 {
		redirectType = http.StatusTemporaryRedirect
	}
	c.Response().Header().Set("Cache-Control", "private, no-store")
	c.Response().Header().Set("X-Mira-Backend", selected.Name)
	c.Response().Header().Set("X-Mira-Route", selectedRoute)
	return c.Redirect(redirectType, fmt.Sprintf("%s%s", selected.URL, resource))
}

func (a *application) status(c echo.Context) error {
	serverStatus := make([]interface{}, 0)
	for _, value := range a.groups.servers {
		serverStatus = append(serverStatus, value.getStatus())
	}
	data, err := json.Marshal(serverStatus)
	if err != nil {
		return err
	}
	return c.String(http.StatusOK, string(data))
}

func (a *application) setCookie(c echo.Context) error {
	group := c.Param("group")
	c.SetCookie(&http.Cookie{
		Name:   "group",
		Value:  group,
		MaxAge: 365 * 24 * 3600,
	})
	return c.NoContent(http.StatusOK)
}

func (a *application) deleteCookie(c echo.Context) error {
	c.SetCookie(&http.Cookie{
		Name:   "group",
		Value:  "",
		MaxAge: -1,
	})
	return c.NoContent(http.StatusOK)
}

func (a *application) listBackends(c echo.Context) error {
	backends := make([]backendMetadata, 0)
	for _, value := range a.groups.servers {
		server, ok := value.(*server)
		if !ok {
			continue
		}
		metadata := server.publicMetadata()
		if metadata.Selectable {
			backends = append(backends, metadata)
		}
	}
	sort.Slice(backends, func(i, j int) bool {
		return backends[i].ID < backends[j].ID
	})
	c.Response().Header().Set("Cache-Control", "private, no-store")
	return c.JSON(http.StatusOK, backendListResponse{Version: 1, Backends: backends})
}

func (a *application) createRoute(c echo.Context) error {
	request, err := decodeRouteRequest(c)
	if err != nil {
		return routeAPIError(c, http.StatusBadRequest, err.Error())
	}
	resource, err := a.routing.normalizeResource(request.Resource)
	if err != nil {
		return routeAPIError(c, http.StatusBadRequest, err.Error())
	}
	if len(request.ExcludeBackendIDs) > maxExcludedBackends {
		return routeAPIError(c, http.StatusBadRequest, "too many excluded backends")
	}
	excluded := make(map[string]struct{}, len(request.ExcludeBackendIDs))
	for _, backendID := range request.ExcludeBackendIDs {
		backendID = strings.TrimSpace(backendID)
		if backendID == "" {
			return routeAPIError(c, http.StatusBadRequest, "excluded backend ID must not be empty")
		}
		excluded[backendID] = struct{}{}
	}

	mode := request.Preference.Mode
	if mode == "" {
		mode = "auto"
	}
	options := serverSelectionOptions{
		excluded:       excluded,
		selectableOnly: true,
	}
	var selected *server
	var reason string
	switch mode {
	case "auto":
		selected, reason = a.groups.selectRouteServer(c, options)
		if reason != "" {
			reason += "-group"
		}
	case "backend":
		if request.Preference.BackendID == "" {
			return routeAPIError(c, http.StatusUnprocessableEntity, "backend preference requires backendId")
		}
		preferred := a.groups.getConcreteServer(request.Preference.BackendID)
		if preferred == nil || !preferred.publicMetadata().Selectable {
			return routeAPIError(c, http.StatusUnprocessableEntity, "preferred backend is not selectable")
		}
		selected = preferred.selectServer(options)
		reason = "manual-preference"
		if selected == nil {
			excluded[request.Preference.BackendID] = struct{}{}
			selected, _ = a.groups.selectRouteServer(c, options)
			reason = "preferred-backend-unavailable"
		}
	default:
		return routeAPIError(c, http.StatusUnprocessableEntity, "preference mode must be auto or backend")
	}
	if selected == nil {
		return routeAPIError(c, http.StatusServiceUnavailable, "no healthy backend is available")
	}

	token, claims, err := a.routing.issueToken(selected.Name, resource)
	if err != nil {
		return err
	}
	playbackURL, err := a.routing.addToken(resource, token)
	if err != nil {
		return err
	}
	playbackURL = a.absolutePlaybackURL(c, playbackURL)
	response := routeResponse{
		RouteToken:      token,
		PlaybackURL:     playbackURL,
		SelectedBackend: selected.publicMetadata(),
		Selection:       routeSelection{Mode: mode, Reason: reason},
		IssuedAt:        time.Unix(claims.IssuedAt, 0).UTC(),
		FreshUntil:      time.Unix(claims.FreshUntil, 0).UTC(),
		ExpiresAt:       time.Unix(claims.ExpiresAt, 0).UTC(),
	}
	c.Response().Header().Set("Cache-Control", "private, no-store")
	return c.JSON(http.StatusCreated, response)
}

func decodeRouteRequest(c echo.Context) (routeRequest, error) {
	requestBody := http.MaxBytesReader(c.Response().Writer, c.Request().Body, maxRouteRequestBodySize)
	decoder := json.NewDecoder(requestBody)
	decoder.DisallowUnknownFields()
	var request routeRequest
	if err := decoder.Decode(&request); err != nil {
		return routeRequest{}, fmt.Errorf("decode route request: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return routeRequest{}, errors.New("route request must contain one JSON object")
	}
	return request, nil
}

func routeAPIError(c echo.Context, status int, message string) error {
	return c.JSON(status, map[string]string{"error": message})
}

func (a *application) routingCORSMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		origin := c.Request().Header.Get("Origin")
		if origin == "" {
			return next(c)
		}
		if !a.isAllowedOrigin(origin, c.Scheme(), c.Request().Host) {
			return routeAPIError(c, http.StatusForbidden, "origin is not allowed")
		}
		header := c.Response().Header()
		header.Set("Access-Control-Allow-Origin", origin)
		header.Add("Vary", "Origin")
		if c.Request().Method == http.MethodOptions {
			header.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			header.Set("Access-Control-Allow-Headers", "Content-Type")
			header.Set("Access-Control-Max-Age", "600")
			return c.NoContent(http.StatusNoContent)
		}
		return next(c)
	}
}

func (a *application) isAllowedOrigin(origin string, requestScheme string, requestHost string) bool {
	normalizedOrigin, err := normalizeHTTPOrigin(origin)
	if err != nil {
		return false
	}
	requestOrigin, err := normalizeHTTPOrigin(requestScheme + "://" + requestHost)
	if err == nil && normalizedOrigin == requestOrigin {
		return true
	}
	for _, allowedOrigin := range a.routing.config.AllowedOrigins {
		if normalizedOrigin == allowedOrigin {
			return true
		}
	}
	return false
}

func (a *application) absolutePlaybackURL(c echo.Context, playbackURL string) string {
	origin := a.routing.config.PublicOrigin
	if origin == "" {
		origin = c.Scheme() + "://" + c.Request().Host
	}
	return strings.TrimSuffix(origin, "/") + playbackURL
}

func (a *application) routeRateLimitMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if c.Request().Method == http.MethodOptions {
			return next(c)
		}
		if !a.limiter.allow(requestPeer(c.Request())) {
			c.Response().Header().Set("Retry-After", "60")
			return routeAPIError(c, http.StatusTooManyRequests, "routing API rate limit exceeded")
		}
		return next(c)
	}
}

func (l *fixedWindowRateLimiter) allow(key string) bool {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	now := l.now()
	if l.lastCleanup.IsZero() || now.Sub(l.lastCleanup) >= l.window {
		l.removeExpired(now)
		l.lastCleanup = now
	}
	value, ok := l.values[key]
	if !ok || now.Sub(value.startedAt) >= l.window {
		if !ok && len(l.values) >= maxRateLimitEntries {
			l.removeExpired(now)
			if len(l.values) >= maxRateLimitEntries {
				return false
			}
		}
		l.values[key] = rateLimitWindow{startedAt: now, requests: 1}
		return true
	}
	if value.requests >= l.limit {
		return false
	}
	value.requests++
	l.values[key] = value
	return true
}

func (l *fixedWindowRateLimiter) removeExpired(now time.Time) {
	for key, value := range l.values {
		if now.Sub(value.startedAt) >= l.window {
			delete(l.values, key)
		}
	}
}

func requestPeer(request *http.Request) string {
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err == nil {
		return host
	}
	return request.RemoteAddr
}
