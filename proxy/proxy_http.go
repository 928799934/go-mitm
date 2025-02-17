package proxy

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	// tr = &http.Transport{
	// 	DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
	// 		dialer := &net.Dialer{
	// 			Timeout:   30 * time.Second,
	// 			KeepAlive: 30 * time.Second,
	// 		}
	// 		conn, err := dialer.DialContext(ctx, network, addr)
	// 		fmt.Println(network, addr, conn.RemoteAddr())
	// 		return conn, err
	// 	},
	// 	DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
	// 		dialer := &tls.Dialer{
	// 			NetDialer: &net.Dialer{
	// 				Timeout:   30 * time.Second,
	// 				KeepAlive: 30 * time.Second,
	// 			},
	// 			Config: &tls.Config{
	// 				InsecureSkipVerify: true,
	// 			},
	// 		}
	// 		conn, err := dialer.DialContext(ctx, network, addr)
	// 		return conn, err
	// 	},
	// 	// MaxIdleConnsPerHost:   1000,
	// 	// MaxConnsPerHost:       1000,
	// 	// IdleConnTimeout:       30 * time.Second,
	// 	// TLSHandshakeTimeout:   30 * time.Second,
	// 	// ExpectContinueTimeout: 30 * time.Second,
	// 	// TLSClientConfig: &tls.Config{
	// 	// 	InsecureSkipVerify: true,
	// 	// },
	// 	ForceAttemptHTTP2: true,
	// 	// DisableKeepAlives: true,
	// 	//DisableCompression: true,
	// }

	reverseProxyPool = &sync.Pool{
		New: func() interface{} {
			return &httputil.ReverseProxy{ /*Transport: tr*/ }
		},
	}
)

func (p *Proxy) doRequest(rw http.ResponseWriter, r *http.Request) {
	var (
		spend    uint16
		size     int64
		respBody string
	)

	reqHeader := make(map[string]string)

	for k := range r.Header {
		reqHeader[k] = r.Header.Get(k)
	}

	reqCookie := make(map[string]string)
	for _, v := range r.Cookies() {
		reqCookie[v.Name] = v.Value
	}

	reqBody, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(rw, "Failed to read request body", http.StatusInternalServerError)
		return
	}
	r.Body = io.NopCloser(bytes.NewBuffer(reqBody))

	reqTls := make(map[string]string)
	if r.TLS != nil {
		reqTls["ServerName"] = r.TLS.ServerName
		reqTls["NegotiatedProtocol"] = r.TLS.NegotiatedProtocol
		reqTls["Version"] = fmt.Sprintf("%d", r.TLS.Version)
		reqTls["Unique"] = string(r.TLS.TLSUnique)
		reqTls["CipherSuite"] = cipherSuiteMap[r.TLS.CipherSuite]
	}

	// 识别 gzip
	acceptEncoding := strings.Split(r.Header.Get("Accept-Encoding"), ",")
	_, bGZIP := dealWithGZIP(acceptEncoding)

	// 获取 代理资源
	reverseProxy := reverseProxyPool.Get().(*httputil.ReverseProxy)
	defer func() {
		reverseProxy.Director = nil
		reverseProxy.ModifyResponse = nil
		reverseProxyPool.Put(reverseProxy)
	}()

	var begin time.Time
	reverseProxy.Rewrite = func(req *httputil.ProxyRequest) {
		begin = time.Now()
		req.Out.Header.Set("HOST", r.Host)
		req.Out.Header.Del("Accept-Encoding")
	}

	tr := http.DefaultTransport.(*http.Transport)
	if p.proxy != nil {
		tr.Proxy = func(_ *http.Request) (*url.URL, error) {
			return p.proxy, nil
		}
	}

	reverseProxy.Transport = tr

	reverseProxy.ErrorHandler = func(resp http.ResponseWriter, req *http.Request, err error) {
		fmt.Printf("ErrorHandler url(%v) error(%v)\n", req.URL.String(), err)
	}

	reverseProxy.ModifyResponse = func(resp *http.Response) error {
		spend = uint16(time.Since(begin).Milliseconds())

		respHeader := make(map[string]string)
		for k := range resp.Header {
			respHeader[k] = resp.Header.Get(k)
		}
		respCookie := make(map[string]string)
		for _, v := range resp.Cookies() {
			respCookie[v.Name] = v.Raw
		}

		contentType := ""
		for _, v := range strings.Split(resp.Header.Get("Content-Type"), ";") {
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

		respTls := make(map[string]string)
		if resp.TLS != nil {
			respTls["ServerName"] = resp.TLS.ServerName
			respTls["NegotiatedProtocol"] = resp.TLS.NegotiatedProtocol
			version := "Unknown"
			switch resp.TLS.Version {
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
			respTls["Unique"] = base64.StdEncoding.EncodeToString(resp.TLS.TLSUnique)
			if r.TLS != nil {
				if cipherSuite, ok := cipherSuiteMap[r.TLS.CipherSuite]; ok {
					respTls["CipherSuite"] = cipherSuite
				}
			}
		}

		// 一次性读取body
		responseData, err := io.ReadAll(resp.Body)
		if err != nil {
			fmt.Printf("io.ReadAll(resp.Body) error(%v)\n", err)
			return err
		}
		resp.Body.Close()

		respBody = string(responseData)
		size = int64(len(responseData))

		if len(responseData) > 0 {
			// gzip 补充
			if bGZIP {
				resp.Header.Set("Content-Encoding", "gzip")
				responseData = withGZIP(responseData)
			}
			// 重新计算 Content-Length
			// resp.Header.Set("Content-Length", strconv.Itoa(len(responseData)))
		}
		// 重写 body
		resp.Body = io.NopCloser(bytes.NewBuffer(responseData))
		if p.messageChan == nil {
			return nil
		}
		p.messageChan <- &Message{
			Url:        r.URL.String(),
			RemoteAddr: r.RemoteAddr,
			Method:     r.Method,
			Type:       contentType,
			Time:       spend,
			Size:       uint16(size),
			Status:     uint16(resp.StatusCode),
			ReqHeader:  reqHeader,
			ReqCookie:  reqCookie,
			ReqBody:    string(reqBody),
			RespHeader: respHeader,
			RespCookie: respCookie,
			RespBody:   respBody,
			RespTls:    respTls,
		}
		return nil
	}

	reverseProxy.ServeHTTP(rw, r)
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if strings.Contains(r.Host, ":") {
		host = host[:strings.Index(host, ":")]
	}
	if len(p.exclude) > 0 {
		for _, v := range p.exclude {
			matched, _ := filepath.Match(v, host)
			if matched {
				p.forward(w, r)
				return
			}
		}
	}

	include := true
	if len(p.include) > 0 {
		include = false
		for _, v := range p.include {
			matched, _ := filepath.Match(v, host)
			if matched {
				include = true
				break
			}
		}
	}

	if !include {
		p.forward(w, r)
		return
	}

	if r.Method == http.MethodConnect {
		p.handleHttps(w, r)
	} else {
		if r.URL.Host == "" {
			r.URL.Host = r.Host
		}

		if r.URL.Scheme == "" {
			r.URL.Scheme = "https"
		}
		for _, v := range p.replace {
			if ok, _ := filepath.Match(v[0], r.URL.String()); ok {
				if v[1] == "https://" || v[1] == "http://" {
					r, err := http.NewRequest("GET", v[2], nil)
					if err == nil {
						p.doReplace1(w, r)
					}
				}
				if v[1] == "file://" {
					data, err := os.ReadFile("/" + v[2])
					if err == nil {
						p.doReplace2(w, data)
					}
				}
				return
			}
		}
		p.doRequest(w, r)
	}
}
