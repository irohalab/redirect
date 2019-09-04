package main

import (
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

func (s *server) init(input interface{}) *server {
	data, ok := input.(map[string]interface{})
	if !ok {
		return nil
	}
	if t, ok := data["URL"]; ok {
		if v, ok := t.(string); ok {
			s.URL = v
		}
	}
	s.RedirectType = 307
	if t, ok := data["RedirectType"]; ok {
		if v, ok := t.(int); ok {
			s.RedirectType = v
		}
	}
	s.Check = true
	if t, ok := data["NoCheck"]; ok {
		if v, ok := t.(bool); ok {
			s.Check = !v
		}
	}
	log.Printf("Regist server %s success.\n", s.Name)
	return s
}

func (s *server) watch() {
	if !s.Check {
		return
	}
	for {
		time.Sleep(10 * time.Second)
		req, err := http.NewRequest(http.MethodGet, s.URL+"/generate_204", nil)
		req.Header.Set("Connection", "close")
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusNoContent {
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
	Servers      map[string]balanceGroup
	sortedWeight []serverWithWeight
	TotalWeight  float64
	mutex        sync.Mutex
}

func (s *group) init(input interface{}) *group {
	data, ok := input.(map[string]interface{})
	if !ok {
		return nil
	}
	s.Type = "fallback"
	if t, ok := data["Type"]; ok {
		if v, ok := t.(string); ok {
			s.Type = v
		}
	}
	s.sortedWeight = make([]serverWithWeight, 0)
	s.Servers = make(map[string]balanceGroup)
	if t, ok := data["Servers"]; ok {
		if v, ok := t.(map[string]interface{}); ok {
			for kk, vv := range v {
				var weight float64
				switch vv.(type) {
				case int:
					weight = float64(vv.(int))
				case float64:
					weight = vv.(float64)
				}
				s.sortedWeight = append(s.sortedWeight, serverWithWeight{kk, weight})
			}
		}
	}
	sort.Slice(s.sortedWeight, func(i, j int) bool { return s.sortedWeight[i].Weight > s.sortedWeight[j].Weight })
	log.Printf("Regist group %s success. \n", s.Name)
	return s
}

func (s *group) constructGroup(m *groupManager) {
	for _, v := range s.sortedWeight {
		server := m.get(v.Name)
		if server == nil {
			log.Fatalf("Construct error. %s not found", v.Name)
		}
		s.Servers[v.Name] = server
	}

}
func (s *group) getServer() *server {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	switch s.Type {
	case "fallback":
		for _, v := range s.sortedWeight {
			t, ok := s.Servers[v.Name]
			if !ok {
				continue
			}
			server := t.getServer()
			if server != nil {
				return server
			}
		}
		return nil
	case "random":
		if len(s.Servers) <= 0 {
			return nil
		}
		maxWeight := s.sortedWeight[0].Weight
		for times := 0; times < 1000; times++ {
			i := rand.Intn(len(s.Servers))
			t := s.sortedWeight[i]
			if rand.Float64()*maxWeight < t.Weight {
				server := s.Servers[t.Name].getServer()
				if server != nil {
					return server
				}
			}
		}
		return s.Servers[s.sortedWeight[0].Name].getServer()
	}
	return nil
}

func (s *group) watch() {}

func (s *group) getStatus() interface{} {
	server := make([]string, 0)
	for k := range s.Servers {
		server = append(server, k)
	}
	return map[string]interface{}{
		"Name":         s.Name,
		"Type":         s.Type,
		"Servers":      server,
		"TotalWeight":  s.TotalWeight,
		"SortedWeight": s.sortedWeight,
	}
}

type groupManager struct {
	serverInput      map[string]interface{}
	groupInput       map[string]interface{}
	servers          map[string]balanceGroup
	ipipDataFilePath string
	city             *datx.City
}

func (s *groupManager) init(input interface{}) *groupManager {
	s.servers = make(map[string]balanceGroup)
	city, err := datx.NewCity(s.ipipDataFilePath)
	if err == nil {
		log.Printf("Load ip databse from %s.\n", s.ipipDataFilePath)
		s.city = city
	}
	inputData, ok := input.(map[string]interface{})
	if !ok {
		log.Fatal("Group Manager init failed.")
	}
	if sInput, ok := inputData["Servers"]; ok {
		s.serverInput = sInput.(map[string]interface{})
	}
	if gInput, ok := inputData["Groups"]; ok {
		s.groupInput = gInput.(map[string]interface{})
	}
	for k := range s.serverInput {
		log.Printf("Construct server %s.\n", k)
		s.get(k)
	}
	for k := range s.groupInput {
		log.Printf("Construct group %s.\n", k)
		s.get(k)
	}
	return s
}

func (s *groupManager) createServer(name string) balanceGroup {
	data, ok := s.serverInput[name]
	if !ok {
		return nil
	}
	server := (&server{Name: name}).init(data)
	if server != nil {
		s.servers[name] = server
	}
	return s.servers[name]
}

func (s *groupManager) createGroup(name string) balanceGroup {
	data, ok := s.groupInput[name]
	if !ok {
		return nil
	}
	group := (&group{Name: name}).init(data)
	if group != nil {
		s.servers[name] = group
		group.constructGroup(s)
	}
	return s.servers[name]
}

func (s *groupManager) getByGroup(c echo.Context) balanceGroup {
	groupCookie, err := c.Cookie("group")
	if err != nil {
		return nil
	}
	groupName := groupCookie.Value
	if group, ok := s.servers[groupName]; ok {
		return group
	}
	return nil
}

func (s *groupManager) getByIp(c echo.Context) balanceGroup {
	ip := c.RealIP()
	location, err := s.city.FindLocation(ip)
	if err != nil {
		log.Println(err)
		return nil
	}
	log.Printf("ip: %s,location: %s.\n", ip, location.Country)
	if v := s.get(location.Country); v != nil {
		return v
	}
	return nil
}

func (s *groupManager) get(name string) balanceGroup {
	if v, ok := s.servers[name]; ok {
		return v
	}
	if _, ok := s.serverInput[name]; ok {
		return s.createServer(name)
	}
	if _, ok := s.groupInput[name]; ok {
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
