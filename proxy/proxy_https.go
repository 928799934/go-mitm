package proxy

import (
	"io"
	"net"
	"net/http"
	"sync"
)

func (p *Proxy) handleHttps(w http.ResponseWriter, r *http.Request) {
	client, server := net.Pipe()
	defer func() {
		_ = client.Close()
	}()

	p.listener.AddConn(NewConn(server, r.RemoteAddr))

	_, _ = w.Write([]byte("HTTP/1.1 200 Connection Established\n\n"))

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "Hijacking not supported", http.StatusInternalServerError)
		return
	}

	hijack, _, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	}
	defer func() {
		_ = hijack.Close()
	}()

	var g sync.WaitGroup
	g.Add(2)
	go func() {
		defer g.Done()
		io.Copy(client, hijack)
	}()
	go func() {
		defer g.Done()
		io.Copy(hijack, client)
	}()
	g.Wait()
}

func (p *Proxy) start() error {
	return p.srv.ServeTLS(p.listener, "", "")
}

func isHttpsRequest(r *http.Request) bool {
	return r.Method == http.MethodConnect
}
