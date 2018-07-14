package main

import (
	"flag"
	"io/ioutil"
	"log"
	"encoding/json"
	"github.com/labstack/echo"
	"github.com/labstack/echo/middleware"
	"strconv"
	"errors"
	"fmt"
)

var gm *groupManager

func handler(c echo.Context) error {
	var group balanceGroup
	group = gm.getByGroup(c)
	if group == nil {
		group = gm.getByIp(c)
	}
	if group == nil {
		group = gm.get("main")
	}
	if group == nil {
		return errors.New("empty main group")
	}
	server := group.getServer()
	if server == nil {
		return errors.New("no server found")
	}
	c.Redirect(server.RedirectType, fmt.Sprintf("%s%s", server.URL, c.Request().URL))
	return nil
}

func main() {
	path := flag.String("c", "./config.json", "Config Path.")
	ipData := flag.String("d", "./data.ipx", "Ipp.net database.")
	port := flag.Int("p", 1080, "Listen Port.")
	flag.Parse()
	data, err := ioutil.ReadFile(*path)
	if err != nil {
		log.Fatal(err)
	}
	gm = &groupManager{ipipDataFilePath: *ipData}
	input := make(map[string]interface{})
	json.Unmarshal(data, &input)
	gm.init(input)
	go gm.watch()

	e := echo.New()
	//e.Use(middleware.Logger())
	e.Use(middleware.Recover())

	e.GET("/*", handler)
	e.Logger.Fatal(e.Start(":" + strconv.Itoa(*port)))
}
