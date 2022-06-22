package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"strconv"

	"github.com/labstack/echo"
	"github.com/labstack/echo/middleware"
)

var gm *groupManager
var verboseMode bool

func handler(c echo.Context) error {
	var group balanceGroup
	group = gm.getByGroup(c)
	if group == nil {
		group = gm.getByIP(c)
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
	if verboseMode {
		cookie, err := c.Cookie("Group")
		if cookie == nil || err != nil {
			log.Printf("Request from [%s] to [%s] has been redirect to [%s]\n", c.RealIP(), c.Request().URL, server.Name)
		} else {
			log.Printf("Request from [%s] to [%s] with cookie [%s] has been redirect to [%s]\n", c.RealIP(), c.Request().URL, cookie.Value, server.Name)
		}
	}
	var redirectType int
	if server.RedirectType != 0 {
		redirectType = server.RedirectType
	} else if gm.RedirectType != 0 {
		redirectType = gm.RedirectType
	} else {
		redirectType = 307
	}
	c.Redirect(redirectType, fmt.Sprintf("%s%s", server.URL, c.Request().URL))
	return nil
}

func status(c echo.Context) error {
	serverStatus := make([]interface{}, 0)
	for _, v := range gm.servers {
		serverStatus = append(serverStatus, v.getStatus())
	}
	data, err := json.Marshal(serverStatus)
	if err != nil {
		return err
	}
	c.String(200, string(data))
	return nil
}

func setCookie(c echo.Context) error {
	group := c.Param("group")
	c.SetCookie(&http.Cookie{
		Name:   "group",
		Value:  group,
		MaxAge: 365 * 24 * 3600,
	})
	return nil
}

func delCookie(c echo.Context) error {
	c.SetCookie(&http.Cookie{
		Name:   "group",
		Value:  "",
		MaxAge: -1,
	})
	return nil
}

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
	gm = (&groupManager{}).init(*ipData, data)
	go gm.watch()

	e := echo.New()
	//e.Use(middleware.Logger())
	e.Use(middleware.Recover())

	e.GET("/*", handler)
	e.POST("/*", status)
	e.PUT("/:group", setCookie)
	e.DELETE("/", delCookie)
	e.Logger.Fatal(e.Start(":" + strconv.Itoa(*port)))
}
