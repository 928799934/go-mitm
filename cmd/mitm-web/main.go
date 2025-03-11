package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/928799934/go-mitm/proxy"
	"github.com/928799934/go-mitm/static"
	"github.com/928799934/go-mitm/web/api"
)

func main() {
	midPortPtr := flag.Int("mid-port", 8082, "-mid-port proxyPort")
	webPortPtr := flag.Int("web-port", 8083, "-web-port webPort")
	includePtr := flag.String("include", "", "-include include")
	excludePtr := flag.String("exclude", "localhost;127.0.0.1", "-exclude exclude")
	proxyPtr := flag.String("proxy", "", "-proxy http://127.0.0.1:1080")
	socks5Ptr := flag.String("socks5", "", "-socks5 127.0.0.1:1080")
	flag.Parse()

	lanIp := proxy.LanIp()
	internetIp := proxy.InternetIp()

	messageChan := make(chan *proxy.Message, 10240)

	p, err := proxy.NewProxy(fmt.Sprintf(":%d", *midPortPtr), static.CaCert, static.CaKey)
	if err != nil {
		panic(err)
	}

	p.SetMessageChan(messageChan)
	p.SetInclude(*includePtr)
	p.SetExclude(*excludePtr)
	p.SetProxy(*proxyPtr)
	p.SetSocks5(*socks5Ptr)

	p.Start()

	fmt.Printf("Mid: http://%s:%d http://%s:%d http://%s:%d\n", "localhost", *midPortPtr, lanIp, *midPortPtr, internetIp, *midPortPtr)

	defer func() {
		p.Stop()
	}()

	handler := api.NewApi(messageChan, lanIp, internetIp, *midPortPtr, p).Handler()
	handler = api.CrossDomain(handler)
	handler = api.Print(handler)
	srvApi := &http.Server{
		Addr:    fmt.Sprintf(":%d", *webPortPtr),
		Handler: handler,
	}
	fmt.Printf("Web: http://%s:%d http://%s:%d http://%s:%d\n", "localhost", *webPortPtr, lanIp, *webPortPtr, internetIp, *webPortPtr)
	go func() {
		err = srvApi.ListenAndServe()
		if err != nil {
			if err != http.ErrServerClosed {
				panic(err)
			}
		}
	}()

	defer func() {
		srvApi.Close()
	}()

	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGHUP, syscall.SIGQUIT,
		syscall.SIGTERM, syscall.SIGINT, syscall.SIGTRAP)
loop:
	for {
		switch <-c {
		case syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGINT:
			break loop
		case syscall.SIGHUP:
		}
	}
}
