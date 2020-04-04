package main

import (
	"encoding/json"
	"log"
	"math/rand"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/ipipdotnet/datx-go"
	"github.com/labstack/echo"
)

type balanceGroup interface {
	getServer() *server
	getStatus() interface{}
	watch()
}

type server struct {
	Name         string
	URL          string
	RedirectType int
	Offline      bool
	Check        bool
	LastOnline   time.Time
	Count        int64
}

func (s *server) init(name string, config []byte) *server {
	s.Name = name
	s.Check = true
	if err := json.Unmarshal(config, s); err != nil {
		log.Fatalf("init server %s failed, err: %s, config: %s\n", name, err.Error(), config)
	}
	log.Printf("Regist server %s success.\n", s.Name)
	return s
}

func (s *server) watch() {
	if s.Check {
		for {
			time.Sleep(10 * time.Second)
			req, _ := http.NewRequest(http.MethodGet, s.URL+"/generate_204", nil)
			req.Header.Set("Connection", "close")
			if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusNoContent {
				if s.Offline == false {
					if err != nil {
						log.Println(err)
					}
					log.Printf("[%s] Offline.\n", s.Name)
				}
				s.Offline = true
			} else {
				if s.Offline == true {
					log.Printf("[%s] back to Online.\n", s.Name)
				}
				s.Offline = false
				s.LastOnline = time.Now()
			}
		}
	}
}

func (s *server) getServer() *server {
	if s.Offline {
		return nil
	}
	s.Count++
	return s
}

func (s *server) getStatus() interface{} {
	return *s
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
	for k, v := range s.Servers {
		s.sortedWeight = append(s.sortedWeight, serverWithWeight{k, v})
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
	s.mutex.Lock()
	defer s.mutex.Unlock()
	switch s.Type {
	case "fallback":
		for _, v := range s.sortedWeight {
			if t, ok := s.servers[v.Name]; ok {
				if server := t.getServer(); server != nil {
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
				if server := s.servers[t.Name].getServer(); server != nil {
					return server
				}
			}
		}
		return s.servers[s.sortedWeight[0].Name].getServer()
	}
	return nil
}

func (s *group) watch() {}

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

func (s *groupManager) watch() {
	wg := sync.WaitGroup{}
	for _, v := range s.servers {
		wg.Add(1)
		go v.watch()
	}
	wg.Wait()
}
