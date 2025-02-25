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
		spend uint16
		size  int64
		// respBody string
	)

	reqHeader := make(map[string]string)

	for k := range r.Header {
		reqHeader[k] = r.Header.Get(k)
	}

	reqCookie := make(map[string]string)
	for _, v := range r.Cookies() {
		reqCookie[v.Name] = v.Value
	}

	reqBody := new(bytes.Buffer)

	r.Body = io.NopCloser(io.TeeReader(r.Body, reqBody))

	// reqBody, err := io.ReadAll(r.Body)
	// if err != nil {
	// 	http.Error(rw, "Failed to read request body", http.StatusInternalServerError)
	// 	return
	// }
	// r.Body = io.NopCloser(bytes.NewBuffer(reqBody))

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
		// 对于流式传输，保留原始的 Accept-Encoding
		if !isStreamResponse(r.Header) {
			req.Out.Header.Del("Accept-Encoding")
		}
	}

	tr := http.DefaultTransport.(*http.Transport)
	if p.proxy != nil {
		tr.Proxy = func(_ *http.Request) (*url.URL, error) {
			return p.proxy, nil
		}
	}
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}

	reverseProxy.Transport = tr

	reverseProxy.ErrorHandler = func(resp http.ResponseWriter, req *http.Request, err error) {
		// fmt.Printf("ErrorHandler url(%v) error(%v)\n", req.URL.String(), err)
	}

	reverseProxy.ModifyResponse = func(resp *http.Response) error {
		spend = uint16(time.Since(begin).Milliseconds())

		// 处理响应头
		respHeader := make(map[string]string)
		for k := range resp.Header {
			respHeader[k] = resp.Header.Get(k)
		}

		respCookie := make(map[string]string)
		for _, v := range resp.Cookies() {
			respCookie[v.Name] = v.Value
		}

		// 检查是否为流式响应
		if isStreamResponse(resp.Header) {
			// 对于流式响应，直接返回，不修改 body
			if p.messageChan != nil {
				p.messageChan <- &Message{
					Url:        r.URL.String(),
					RemoteAddr: r.RemoteAddr,
					Method:     r.Method,
					Type:       getContentType(resp.Header),
					Time:       spend,
					Status:     uint16(resp.StatusCode),
					ReqHeader:  reqHeader,
					ReqCookie:  reqCookie,
					ReqBody:    reqBody.String(),
					RespHeader: respHeader,
					RespCookie: respCookie,
					RespTls:    getTLSInfo(resp.TLS, r.TLS),
				}
			}
			return nil
		}

		// 非流式响应的处理保持不变

		respBody := new(bytes.Buffer)

		resp.Body = io.NopCloser(io.TeeReader(resp.Body, respBody))

		responseData, err := io.ReadAll(resp.Body)
		if err != nil {
			fmt.Printf("io.ReadAll(resp.Body) error(%v)\n", err)
			return err
		}

		// respBody = string(responseData)
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
			Type:       getContentType(resp.Header),
			Time:       spend,
			Size:       uint16(size),
			Status:     uint16(resp.StatusCode),
			ReqHeader:  reqHeader,
			ReqCookie:  reqCookie,
			ReqBody:    reqBody.String(),
			RespHeader: respHeader,
			RespCookie: respCookie,
			RespBody:   respBody.String(),
			RespTls:    getTLSInfo(resp.TLS, r.TLS),
		}
		return nil
	}

	reverseProxy.ServeHTTP(rw, r)
}

// 新增：检查是否为流式响应
func isStreamResponse(header http.Header) bool {
	// 检查 WebSocket
	if strings.ToLower(header.Get("Connection")) == "upgrade" &&
		strings.ToLower(header.Get("Upgrade")) == "websocket" {
		return true
	}

	// 检查 Transfer-Encoding
	if header.Get("Transfer-Encoding") == "chunked" {
		return true
	}

	// 检查 Content-Type
	contentType := header.Get("Content-Type")
	return strings.Contains(contentType, "text/event-stream") ||
		strings.Contains(contentType, "application/x-ndjson") ||
		strings.Contains(contentType, "multipart/x-mixed-replace")
}

// 新增：提取 Content-Type
func getContentType(header http.Header) string {
	contentType := header.Get("Content-Type")
	for _, v := range strings.Split(contentType, ";") {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if strings.Contains(strings.ToLower(v), "charset=") {
			continue
		}
		return v
	}
	return ""
}

// 新增：获取 TLS 信息
func getTLSInfo(respTLS, reqTLS *tls.ConnectionState) map[string]string {
	tlsInfo := make(map[string]string)
	if respTLS != nil {
		tlsInfo["ServerName"] = respTLS.ServerName
		tlsInfo["NegotiatedProtocol"] = respTLS.NegotiatedProtocol
		version := "Unknown"
		switch respTLS.Version {
		case tls.VersionTLS10:
			version = "1.0"
		case tls.VersionTLS11:
			version = "1.1"
		case tls.VersionTLS12:
			version = "1.2"
		case tls.VersionTLS13:
			version = "1.3"
		}
		tlsInfo["Version"] = version
		tlsInfo["Unique"] = base64.StdEncoding.EncodeToString(respTLS.TLSUnique)
		if reqTLS != nil {
			if cipherSuite, ok := cipherSuiteMap[reqTLS.CipherSuite]; ok {
				tlsInfo["CipherSuite"] = cipherSuite
			}
		}
	}
	return tlsInfo
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

	if isHttpsRequest(r) {
		p.handleHttps(w, r)
		return
	}

	// 检查是否为 WebSocket 请求
	if isWebSocketRequest(r) {
		p.handleWebSocket(w, r)
		return
	}

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
