package proxy

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/andybalholm/brotli"
	"golang.org/x/net/proxy"
)

var cipherSuiteMap = map[uint16]string{
	0x0005: "TLS_RSA_WITH_RC4_128_SHA",
	0x000a: "TLS_RSA_WITH_3DES_EDE_CBC_SHA",
	0x002f: "TLS_RSA_WITH_AES_128_CBC_SHA",
	0x0035: "TLS_RSA_WITH_AES_256_CBC_SHA",
	0x003c: "TLS_RSA_WITH_AES_128_CBC_SHA256",
	0x009c: "TLS_RSA_WITH_AES_128_GCM_SHA256",
	0x009d: "TLS_RSA_WITH_AES_256_GCM_SHA384",
	0xc007: "TLS_ECDHE_ECDSA_WITH_RC4_128_SHA",
	0xc009: "TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA",
	0xc00a: "TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA",
	0xc011: "TLS_ECDHE_RSA_WITH_RC4_128_SHA",
	0xc012: "TLS_ECDHE_RSA_WITH_3DES_EDE_CBC_SHA",
	0xc013: "TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA",
	0xc014: "TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA",
	0xc023: "TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA256",
	0xc027: "TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA256",
	0xc02f: "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
	0xc02b: "TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256",
	0xc030: "TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384",
	0xc02c: "TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384",
	0xcca8: "TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256",
	0xcca9: "TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256",
	0x1301: "TLS_AES_128_GCM_SHA256",
	0x1302: "TLS_AES_256_GCM_SHA384",
	0x1303: "TLS_CHACHA20_POLY1305_SHA256",
}

var (
	proxyFunc  func(*http.Request) (*url.URL, error)
	socks5Func func(ctx context.Context, network, addr string) (net.Conn, error)
	hookFunc   func(*url.URL, []byte) []byte
)

type Proxy struct {
	wg           sync.WaitGroup
	rootCert     *x509.Certificate
	rootKey      *rsa.PrivateKey
	privateKey   *rsa.PrivateKey
	listener     *Listener
	proxy        string
	socks5       string
	httpSrv      *http.Server
	httpsSrv     *http.Server
	serialNumber int64
	messageChan  chan *Message
	exclude      []string
	include      []string
	replace      [][]string
	logger       *slog.Logger
}

func (p *Proxy) SetMessageChan(messageChan chan *Message) {
	p.messageChan = messageChan
}

func (p *Proxy) getCertificate(domain string) (cert *tls.Certificate, err error) {
	atomic.AddInt64(&p.serialNumber, 1)
	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(p.serialNumber),
		Subject: pkix.Name{
			CommonName: domain,
		},
		NotBefore: time.Now().AddDate(0, 0, -1),
		NotAfter:  time.Now().AddDate(1, 0, 0),
	}
	ip := net.ParseIP(domain)
	if ip != nil {
		serverTemplate.IPAddresses = []net.IP{ip}
	} else {
		serverTemplate.DNSNames = []string{domain}
	}
	certBytes, err := x509.CreateCertificate(rand.Reader, serverTemplate, p.rootCert, &p.privateKey.PublicKey, p.rootKey)
	if err != nil {
		return
	}

	cert = &tls.Certificate{
		PrivateKey:  p.privateKey,
		Certificate: [][]byte{certBytes},
	}
	return
}

func (p *Proxy) doReplace1(w http.ResponseWriter, r *http.Request) {
	tr := HttpTransport()
	response, err := tr.RoundTrip(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	defer func() {
		_ = response.Body.Close()
	}()

	w.WriteHeader(response.StatusCode)
	copyHeader(w.Header(), response.Header)
	_, _ = io.Copy(w, response.Body)
}
func (p *Proxy) doReplace2(w http.ResponseWriter, body []byte) {
	w.WriteHeader(200)
	_, _ = w.Write(body)
}

func (p *Proxy) forward(w http.ResponseWriter, r *http.Request) {
	r.Header.Del("Proxy-Connection")

	if r.Method == "CONNECT" {
		ctx := context.Background()
		ctx, cancel := context.WithTimeout(ctx, time.Second*30)
		defer cancel()

		conn, err := new(net.Dialer).DialContext(ctx, "tcp", r.Host)
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		defer func() {
			_ = conn.Close()
		}()

		_, _ = w.Write([]byte("HTTP/1.1 200 Connection Established\n\n"))
		// if _, err = fmt.Fprint(w, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		// 	http.Error(w, err.Error(), http.StatusServiceUnavailable)
		// 	return
		// }

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
			io.Copy(conn, hijack)
		}()
		go func() {
			defer g.Done()
			io.Copy(hijack, conn)
		}()
		g.Wait()
	} else {
		reverseProxy := reverseProxyPool.Get().(*httputil.ReverseProxy)
		defer func() {
			reverseProxy.Rewrite = nil
			reverseProxy.ModifyResponse = nil
			reverseProxyPool.Put(reverseProxy)
		}()

		reverseProxy.Rewrite = func(req *httputil.ProxyRequest) {
			// req.Out.Header.Set("HOST", r.Host)
			// req.Out.Header.Del("Accept-Encoding")
		}

		reverseProxy.Transport = HttpTransport()
		reverseProxy.ServeHTTP(w, r)

		// response, err := t.RoundTrip(r)
		// if err != nil {
		// 	http.Error(w, err.Error(), http.StatusServiceUnavailable)
		// 	return
		// }

		// defer func() {
		// 	_ = response.Body.Close()
		// }()

		// w.WriteHeader(response.StatusCode)
		// copyHeader(w.Header(), response.Header)
		// _, _ = io.Copy(w, response.Body)
		return
	}
}
func (p *Proxy) Include() []string {
	return p.include
}

func (p *Proxy) SetInclude(includes string) []string {
	include := make([]string, 0)
	for _, v := range strings.Split(includes, ";") {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		include = append(include, v)
	}
	p.include = include
	return p.include
}

func (p *Proxy) ClearInclude() []string {
	p.include = make([]string, 0)
	return p.include
}

func (p *Proxy) Exclude() []string {
	return p.exclude
}

func (p *Proxy) SetExclude(excludes string) []string {
	exclude := make([]string, 0)
	for _, v := range strings.Split(excludes, ";") {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		exclude = append(exclude, v)
	}
	p.exclude = exclude
	return p.exclude
}

func (p *Proxy) ClearExclude() []string {
	p.exclude = make([]string, 0)
	return p.exclude
}

func (p *Proxy) Replace() [][]string {
	return p.replace
}

func (p *Proxy) SetReplace(replaces string) [][]string {
	replace := make([][]string, 0)
	for _, v := range strings.Split(replaces, ";") {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		replace = append(replace, strings.Split(v, ","))
	}
	p.replace = replace
	return p.replace
}

func (p *Proxy) ClearReplace() [][]string {
	p.replace = make([][]string, 0)
	return p.replace
}

func (p *Proxy) Proxy() string {
	return p.proxy
}

func (p *Proxy) SetProxy(uri string) {
	if uri == "" {
		return
	}
	p.proxy = uri
	proxyFunc = func(_ *http.Request) (*url.URL, error) {
		return url.Parse(uri)
	}
}

func (p *Proxy) ClearProxy() {
	p.proxy = ""
	proxyFunc = nil
}

func (p *Proxy) SetSocks5(addr string) {
	if addr == "" {
		return
	}

	p.socks5 = addr

	// u, _ := url.Parse(socks5Proxy)
	// p.socks5Dialer, _ = proxy.FromURL(u, proxy.Direct)
	dialer, _ := proxy.SOCKS5("tcp", addr, nil, proxy.Direct)
	socks5Func = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return dialer.Dial(network, addr)
	}
}

func (p *Proxy) ClearSocks5() {
	p.socks5 = ""
	socks5Func = nil
}

// hookFunc   func(*url.URL, []byte) []byte
func (p *Proxy) SetHook(hook func(*url.URL, []byte) []byte) {
	hookFunc = hook
}

func (p *Proxy) ClearHook() {
	hookFunc = nil
}

func (p *Proxy) Replay(message Message) {
	r, err := http.NewRequest(message.Method, message.Url, strings.NewReader(message.ReqBody))
	if err != nil {
		return
	}
	for k, v := range message.ReqHeader {
		r.Header.Set(k, v)
	}

	reqBody, err := io.ReadAll(r.Body)
	if err != nil {
		return
	}
	r.Body = io.NopCloser(bytes.NewBuffer(reqBody))

	begin := time.Now()
	tr := HttpTransport()
	response, err := tr.RoundTrip(r)
	spend := uint16(time.Since(begin).Milliseconds())
	if err != nil {
		return
	}

	defer func() {
		_ = response.Body.Close()
	}()

	var size int64
	var respBody string
	contentTypes := response.Header.Get("Content-Type")
	bodyBytes, err := io.ReadAll(response.Body)
	if err != nil {
		return
	}

	size = int64(len(bodyBytes))
	if !(strings.Contains(strings.ToLower(contentTypes), "image") || strings.Contains(strings.ToLower(contentTypes), "video")) {
		if response.Header.Get("Content-Encoding") == "deflate" {
			reader := flate.NewReader(bytes.NewReader(bodyBytes))
			defer func() {
				err = reader.Close()
				if err != nil {
					return
				}
			}()

			bodyBytes, err = io.ReadAll(reader)
			if err != nil {
				return
			}
		}
		if response.Header.Get("Content-Encoding") == "br" {
			bodyBytes, err = io.ReadAll(brotli.NewReader(bytes.NewReader(bodyBytes)))
			if err != nil {
				return
			}
		}
		if response.Header.Get("Content-Encoding") == "deflate" {
			reader := flate.NewReader(bytes.NewReader(bodyBytes))
			defer func() {
				if reader != nil {
					err = reader.Close()
					if err != nil {
						return
					}
				}
			}()
			bodyBytes, err = io.ReadAll(reader)
			if err != nil {
				return
			}
		}
		if response.Header.Get("Content-Encoding") == "gzip" {
			reader, err := gzip.NewReader(bytes.NewReader(bodyBytes))
			defer func() {
				if reader != nil {
					err = reader.Close()
					if err != nil {
						return
					}
				}
			}()
			if err != nil {
				return
			}
			bodyBytes, err = io.ReadAll(reader)
			if err != nil {
				return
			}
		}

		respBody = string(bodyBytes)
	}

	go func(r *http.Request, response *http.Response) {
		reqHeader := make(map[string]string)
		for k := range r.Header {
			reqHeader[k] = r.Header.Get(k)
		}
		respHeader := make(map[string]string)
		for k := range response.Header {
			respHeader[k] = response.Header.Get(k)
		}

		//reqTrailer := make(map[string]string)
		//for k := range r.Trailer {
		//	reqTrailer[k] = r.Trailer.Get(k)
		//}
		//respTrailer := make(map[string]string)
		//for k := range response.Trailer {
		//	respTrailer[k] = response.Trailer.Get(k)
		//}

		reqCookie := make(map[string]string)
		for _, v := range r.Cookies() {
			reqCookie[v.Name] = v.Raw
		}
		respCookie := make(map[string]string)
		for _, v := range response.Cookies() {
			respCookie[v.Name] = v.Raw
		}

		reqTls := make(map[string]string)
		if r.TLS != nil {
			reqTls["ServerName"] = r.TLS.ServerName
			reqTls["NegotiatedProtocol"] = r.TLS.NegotiatedProtocol
			reqTls["Version"] = fmt.Sprintf("%d", r.TLS.Version)
			reqTls["Unique"] = string(r.TLS.TLSUnique)
			reqTls["CipherSuite"] = cipherSuiteMap[r.TLS.CipherSuite]
		}

		respTls := make(map[string]string)
		if response.TLS != nil {
			respTls["ServerName"] = response.TLS.ServerName
			respTls["NegotiatedProtocol"] = response.TLS.NegotiatedProtocol
			version := "Unknown"
			switch response.TLS.Version {
			case tls.VersionTLS10:
				version = "1.0"
			case tls.VersionTLS11:
				version = "1.1"
			case tls.VersionTLS12:
				version = "1.2"
			case tls.VersionTLS13:
				version = "1.3"
			}
			respTls["Version"] = version
			respTls["Unique"] = base64.StdEncoding.EncodeToString(response.TLS.TLSUnique)
			if r.TLS != nil {
				if cipherSuite, ok := cipherSuiteMap[r.TLS.CipherSuite]; ok {
					respTls["CipherSuite"] = cipherSuite
				}
			}
		}

		contentType := contentTypes
		for _, v := range strings.Split(contentTypes, ";") {
			v = strings.TrimSpace(v)
			if v == "" {
				continue
			}
			if strings.Contains(strings.ToLower(v), "charset=") {
				continue
			}
			contentType = v
			break
		}

		//p.logger.Info("Response", "StatusCode", response.StatusCode, r.Method, r.URL.String(), "contentType", contentType)

		if p.messageChan != nil {
			p.messageChan <- &Message{
				Url:        r.URL.String(),
				RemoteAddr: r.RemoteAddr,
				Method:     r.Method,
				Type:       contentType,
				Time:       spend,
				Size:       uint16(size),
				Status:     uint16(response.StatusCode),
				ReqHeader:  reqHeader,
				ReqCookie:  reqCookie,
				ReqBody:    string(reqBody),
				RespHeader: respHeader,
				RespCookie: respCookie,
				RespBody:   respBody,
				RespTls:    respTls,
			}
		}
	}(r, response)
}

func copyHeader(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func NewProxy(addr string, caCert, caKey []byte) (p *Proxy, err error) {
	p = new(Proxy)
	p.logger = slog.Default()

	{ // ca.cert
		block, _ := pem.Decode(caCert)
		if block == nil {
			return
		}

		if p.rootCert, err = x509.ParseCertificate(block.Bytes); err != nil {
			return
		}
	}

	{ // ca.key
		block, _ := pem.Decode(caKey)
		if block == nil {
			return
		}

		if p.rootKey, err = x509.ParsePKCS1PrivateKey(block.Bytes); err != nil {
			return
		}
	}

	// server.key
	if p.privateKey, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
		return
	}

	p.listener, _ = NewListener()

	p.httpsSrv = &http.Server{
		Handler: p,
		TLSConfig: &tls.Config{
			GetCertificate: func(info *tls.ClientHelloInfo) (*tls.Certificate, error) {
				return p.getCertificate(info.ServerName)
			},
		},
	}

	p.httpSrv = &http.Server{
		Addr:         addr,
		Handler:      p,
		TLSNextProto: make(map[string]func(*http.Server, *tls.Conn, http.Handler)),
	}
	return
}

func (p *Proxy) Start() {
	p.wg.Add(2)
	go func() {
		defer p.wg.Done()
		p.httpsSrv.ServeTLS(p.listener, "", "")
	}()

	go func() {
		defer p.wg.Done()
		p.httpSrv.ListenAndServe()
	}()

}

func (p *Proxy) Stop() {
	p.httpsSrv.Close()
	p.httpSrv.Close()

	p.wg.Wait()
}
