package main

import (
	"flag"
	"io/ioutil"
	"log"
	"strconv"

	"github.com/labstack/echo"
	"github.com/labstack/echo/middleware"
)

var verboseMode bool

func main() {
	path := flag.String("c", "./config.json", "Config Path.")
	ipData := flag.String("d", "./data.ipx", "Ipp.net database.")
	port := flag.Int("p", 1080, "Listen Port.")
	verbose := flag.Bool("verbose", false, "Verbose mode.")
	flag.Parse()
	verboseMode = *verbose
	data, err := ioutil.ReadFile(*path)
	if err != nil {
		log.Fatal(err)
	}
	groups := (&groupManager{}).init(*ipData, data)
	routing, err := newRoutingService(groups.Routing)
	if err != nil {
		log.Fatal("initialize routing: ", err)
	}
	application := newApplication(groups, routing)
	groups.watch()

	e := echo.New()
	//e.Use(middleware.Logger())
	e.Use(middleware.Recover())

	registerRoutes(e, application)
	e.Logger.Fatal(e.Start(":" + strconv.Itoa(*port)))
}
