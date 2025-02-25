package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/lizongying/go-mitm/proxy"
	"github.com/lizongying/go-mitm/web/api"
)

func main() {
	midPortPtr := flag.Int("mid-port", 8082, "-mid-port proxyPort")
	webPortPtr := flag.Int("web-port", 8083, "-web-port webPort")
	includePtr := flag.String("include", "", "-include include")
	excludePtr := flag.String("exclude", "localhost;127.0.0.1", "-exclude exclude")
	proxyPtr := flag.String("proxy", "", "-proxy proxy")
	flag.Parse()

	lanIp := proxy.LanIp()
	internetIp := proxy.InternetIp()

	messageChan := make(chan *proxy.Message, 255)

	p, err := proxy.NewProxy(*includePtr, *excludePtr, *proxyPtr)
	if err != nil {
		panic(err)
	}
	p.SetMessageChan(messageChan)
	midSrv := &http.Server{
		Addr:         fmt.Sprintf(":%d", *midPortPtr),
		Handler:      p,
		TLSNextProto: make(map[string]func(*http.Server, *tls.Conn, http.Handler)),
	}
	fmt.Printf("Mid: http://%s:%d http://%s:%d http://%s:%d\n", "localhost", *midPortPtr, lanIp, *midPortPtr, internetIp, *midPortPtr)
	go func() {
		err = midSrv.ListenAndServe()
		if err != nil {
			if err != http.ErrServerClosed {
				panic(err)
			}
		}
	}()

	defer func() {
		p.Close()
		midSrv.Close()
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
		// ctx, _ := context.WithTimeout(context.TODO(), 10)
		// srvApi.Shutdown(ctx)
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
