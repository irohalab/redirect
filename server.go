package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ipipdotnet/datx-go"
	"github.com/labstack/echo"
)

type balanceGroup interface {
	getServer() *server
	selectServer(options serverSelectionOptions) *server
	getStatus() interface{}
}

const (
	healthCheckInterval = 10 * time.Second
	healthCheckTimeout  = 5 * time.Second
)

var healthCheckClient = &http.Client{Timeout: healthCheckTimeout}

type serverSelectionOptions struct {
	excluded       map[string]struct{}
	count          bool
	selectableOnly bool
}

func (o serverSelectionOptions) isExcluded(name string) bool {
	_, ok := o.excluded[name]
	return ok
}

type server struct {
	Name         string
	URL          string
	Label        string
	Region       string
	Selectable   bool
	RedirectType int
	Offline      bool
	Check        bool
	LastOnline   time.Time
	Count        int64
	mutex        sync.RWMutex
}

func (s *server) init(name string, config []byte) *server {
	s.Name = name
	s.Check = true
	s.Selectable = true
	if err := json.Unmarshal(config, s); err != nil {
		log.Fatalf("init server %s failed, err: %s, config: %s\n", name, err.Error(), config)
	}
	if err := validateServerURL(s.URL); err != nil {
		log.Fatalf("init server %s failed: %s\n", name, err)
	}
	if s.Label == "" {
		s.Label = s.Name
	}
	log.Printf("Regist server %s success.\n", s.Name)
	return s
}

func (s *server) watch() {
	if !s.Check {
		return
	}
	for {
		time.Sleep(healthCheckInterval)
		online, err := s.checkHealth(healthCheckClient)
		s.updateHealthStatus(online, err)
	}
}

func (s *server) updateHealthStatus(online bool, checkErr error) {
	s.mutex.Lock()
	wasOffline := s.Offline
	s.Offline = !online
	if online {
		s.LastOnline = time.Now()
	}
	s.mutex.Unlock()

	if online && wasOffline {
		log.Printf("[%s] back to Online.\n", s.Name)
		return
	}
	if !online && !wasOffline {
		if checkErr != nil {
			log.Println(checkErr)
		}
		log.Printf("[%s] Offline.\n", s.Name)
	}
}

func validateServerURL(value string) error {
	if value == "" || strings.TrimSpace(value) != value {
		return errors.New("server URL must not be empty or contain surrounding whitespace")
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("parse server URL: %w", err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return errors.New("server URL must contain an http or https scheme and host")
	}
	if parsed.ForceQuery || parsed.RawQuery != "" || strings.Contains(value, "#") {
		return errors.New("server URL must not contain a query string or fragment")
	}
	return nil
}

func (s *server) checkHealth(client *http.Client) (bool, error) {
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(s.URL, "/")+"/generate_204", nil)
	if err != nil {
		return false, fmt.Errorf("create health check request: %w", err)
	}
	req.Header.Set("Connection", "close")
	resp, err := client.Do(req)
	if resp != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		return false, fmt.Errorf("perform health check: %w", err)
	}
	if resp.StatusCode != http.StatusNoContent {
		return false, fmt.Errorf("health check returned status %d", resp.StatusCode)
	}
	return true, nil
}

func (s *server) getServer() *server {
	return s.selectServer(serverSelectionOptions{count: true})
}

func (s *server) selectServer(options serverSelectionOptions) *server {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if s.Offline || options.isExcluded(s.Name) || (options.selectableOnly && !s.Selectable) {
		return nil
	}
	if options.count {
		s.Count++
	}
	return s
}

func (s *server) getStatus() interface{} {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	return struct {
		Name         string
		URL          string
		RedirectType int
		Offline      bool
		Check        bool
		LastOnline   time.Time
		Count        int64
	}{
		Name:         s.Name,
		URL:          s.URL,
		RedirectType: s.RedirectType,
		Offline:      s.Offline,
		Check:        s.Check,
		LastOnline:   s.LastOnline,
		Count:        s.Count,
	}
}

func (s *server) publicMetadata() backendMetadata {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	availability := "healthy"
	if s.Offline {
		availability = "offline"
	}
	return backendMetadata{
		ID:           s.Name,
		Label:        s.Label,
		Region:       s.Region,
		Availability: availability,
		Selectable:   s.Selectable,
	}
}

type serverWithWeight struct {
	Name   string
	Weight float64
}

type group struct {
	Name         string
	Type         string
	Servers      map[string]float64
	servers      map[string]balanceGroup
	sortedWeight []serverWithWeight
	totalWeight  float64
	mutex        sync.Mutex
}

func (s *group) init(name string, config []byte) *group {
	s.Name = name
	s.Type = "fallback"
	if err := json.Unmarshal(config, s); err != nil {
		log.Fatalf("init group %s failed, err: %s, config: %s\n", name, err.Error(), config)
	}
	s.sortedWeight = make([]serverWithWeight, 0)
	s.servers = make(map[string]balanceGroup)
	s.totalWeight = 0
	for k, v := range s.Servers {
		s.sortedWeight = append(s.sortedWeight, serverWithWeight{k, v})
		s.totalWeight += v
	}
	sort.Slice(s.sortedWeight, func(i, j int) bool { return s.sortedWeight[i].Weight > s.sortedWeight[j].Weight })
	log.Printf("Regist group %s success. \n", s.Name)
	return s
}

func (s *group) constructGroup(m *groupManager) {
	for _, v := range s.sortedWeight {
		if server := m.get(v.Name); server != nil {
			s.servers[v.Name] = server
		} else {
			log.Fatalf("Construct error. %s not found", v.Name)
		}
	}
}

func (s *group) getServer() *server {
	return s.selectServer(serverSelectionOptions{count: true})
}

func (s *group) selectServer(options serverSelectionOptions) *server {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	switch s.Type {
	case "fallback":
		for _, v := range s.sortedWeight {
			if t, ok := s.servers[v.Name]; ok {
				if server := t.selectServer(options); server != nil {
					return server
				}
			}
		}
		return nil
	case "random":
		if len(s.servers) <= 0 {
			return nil
		}
		maxWeight := s.sortedWeight[0].Weight
		for times := 0; times < 1000; times++ {
			i := rand.Intn(len(s.Servers))
			t := s.sortedWeight[i]
			if rand.Float64()*maxWeight < t.Weight {
				if server := s.servers[t.Name].selectServer(options); server != nil {
					return server
				}
			}
		}
		for _, weightedServer := range s.sortedWeight {
			if server := s.servers[weightedServer.Name].selectServer(options); server != nil {
				return server
			}
		}
		return nil
	}
	return nil
}

func (s *group) getStatus() interface{} {
	server := make([]string, 0)
	for k := range s.servers {
		server = append(server, k)
	}
	return map[string]interface{}{
		"Name":         s.Name,
		"Type":         s.Type,
		"Servers":      server,
		"TotalWeight":  s.totalWeight,
		"SortedWeight": s.sortedWeight,
	}
}

type groupManager struct {
	Servers          map[string]json.RawMessage
	Groups           map[string]json.RawMessage
	Routing          routingConfig
	servers          map[string]balanceGroup
	ipipDataFilePath string
	RedirectType     int
	city             *datx.City
}

func (s *groupManager) init(ipipDataFilePath string, config []byte) *groupManager {
	s.ipipDataFilePath = ipipDataFilePath
	s.servers = make(map[string]balanceGroup)
	if city, err := datx.NewCity(s.ipipDataFilePath); err != nil {
		log.Fatalf("Load ip database from %s failed: %s\n", s.ipipDataFilePath, err)
	} else {
		log.Printf("Load ip databse from %s.\n", s.ipipDataFilePath)
		s.city = city
	}
	if err := json.Unmarshal([]byte(config), s); err != nil {
		log.Fatal("load config failed: ", err.Error())
	}
	if err := validateGroupConfigurations(s.Servers, s.Groups); err != nil {
		log.Fatal("validate groups: ", err)
	}
	for k := range s.Servers {
		log.Printf("Construct server %s.\n", k)
		s.get(k)
	}
	for k := range s.Groups {
		log.Printf("Construct group %s.\n", k)
		s.get(k)
	}
	return s
}

func (s *groupManager) createServer(name string) balanceGroup {
	if config, ok := s.Servers[name]; ok {
		server := (&server{}).init(name, config)
		if server != nil {
			s.servers[name] = server
		}
		return s.servers[name]
	}
	return nil
}

func (s *groupManager) createGroup(name string) balanceGroup {
	if config, ok := s.Groups[name]; ok {
		group := (&group{}).init(name, config)
		if group != nil {
			s.servers[name] = group
			group.constructGroup(s)
		}
		return s.servers[name]
	}
	return nil
}

func (s *groupManager) getByGroup(c echo.Context) balanceGroup {
	if groupCookie, err := c.Cookie("group"); err == nil {
		groupName := groupCookie.Value
		if group, ok := s.servers[groupName]; ok {
			return group
		}
	}
	return nil
}

func (s *groupManager) getByIP(c echo.Context) balanceGroup {
	if s.city == nil {
		return nil
	}
	ip := c.RealIP()
	if location, err := s.city.FindLocation(ip); err != nil {
		log.Println(err)
		return nil
	} else {
		log.Printf("ip: %s,location: %s.\n", ip, location.Country)
		if v := s.get(location.Country); v != nil {
			return v
		}
	}
	return nil
}

func (s *groupManager) get(name string) balanceGroup {
	if v, ok := s.servers[name]; ok {
		return v
	}
	if _, ok := s.Servers[name]; ok {
		return s.createServer(name)
	}
	if _, ok := s.Groups[name]; ok {
		return s.createGroup(name)
	}
	return nil
}

func (s *groupManager) getConcreteServer(name string) *server {
	value := s.get(name)
	server, ok := value.(*server)
	if !ok {
		return nil
	}
	return server
}

func (s *groupManager) selectRouteServer(c echo.Context, options serverSelectionOptions) (*server, string) {
	if group := s.getByGroup(c); group != nil {
		if selected := group.selectServer(options); selected != nil {
			return selected, "cookie"
		}
	}
	if group := s.getByIP(c); group != nil {
		if selected := group.selectServer(options); selected != nil {
			return selected, "country"
		}
	}
	if group := s.get("main"); group != nil {
		if selected := group.selectServer(options); selected != nil {
			return selected, "main"
		}
	}
	return nil, ""
}

func (s *groupManager) watch() {
	for _, value := range s.servers {
		if backend, ok := value.(*server); ok {
			go backend.watch()
		}
	}
}

type groupConfiguration struct {
	Servers map[string]float64
}

func validateGroupConfigurations(serverConfigs map[string]json.RawMessage, groupConfigs map[string]json.RawMessage) error {
	groups := make(map[string]groupConfiguration, len(groupConfigs))
	for name, rawConfig := range groupConfigs {
		if _, exists := serverConfigs[name]; exists {
			return fmt.Errorf("name %q is configured as both a server and a group", name)
		}
		var config groupConfiguration
		if err := json.Unmarshal(rawConfig, &config); err != nil {
			return fmt.Errorf("decode group %q: %w", name, err)
		}
		for member, weight := range config.Servers {
			if weight <= 0 || math.IsNaN(weight) || math.IsInf(weight, 0) {
				return fmt.Errorf("group %q member %q has invalid weight %v", name, member, weight)
			}
			if _, isServer := serverConfigs[member]; !isServer {
				if _, isGroup := groupConfigs[member]; !isGroup {
					return fmt.Errorf("group %q references unknown member %q", name, member)
				}
			}
		}
		groups[name] = config
	}

	states := make(map[string]uint8, len(groups))
	path := make([]string, 0, len(groups))
	var visit func(string) error
	visit = func(name string) error {
		states[name] = 1
		path = append(path, name)
		for member := range groups[name].Servers {
			if _, isGroup := groups[member]; !isGroup {
				continue
			}
			switch states[member] {
			case 0:
				if err := visit(member); err != nil {
					return err
				}
			case 1:
				cycleStart := 0
				for index, pathMember := range path {
					if pathMember == member {
						cycleStart = index
						break
					}
				}
				cycle := append(append([]string(nil), path[cycleStart:]...), member)
				return fmt.Errorf("group reference cycle: %s", strings.Join(cycle, " -> "))
			}
		}
		path = path[:len(path)-1]
		states[name] = 2
		return nil
	}
	for name := range groups {
		if states[name] == 0 {
			if err := visit(name); err != nil {
				return err
			}
		}
	}
	return nil
}
