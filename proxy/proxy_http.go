package proxy

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	tr = &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		// DialContext: (&net.Dialer{
		// 	Timeout:   30 * time.Second,
		// 	KeepAlive: 30 * time.Second,
		// }).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
	}

	reverseProxyPool = &sync.Pool{
		New: func() any {
			return &httputil.ReverseProxy{ /*Transport: tr*/ }
		},
	}
)

func HttpTransport() http.RoundTripper {
	if socks5Func != nil && tr.DialContext == nil {
		tr.DialContext = socks5Func
	}
	if socks5Func == nil && tr.DialContext != nil {
		tr.DialContext = nil
	}
	if proxyFunc != nil && tr.Proxy == nil {
		tr.Proxy = proxyFunc
	}
	if proxyFunc == nil && tr.Proxy != nil {
		tr.Proxy = nil
	}

	return tr
}

func (p *Proxy) doRequest(rw http.ResponseWriter, r *http.Request) {

	var (
		spend uint16
		size  int64
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
		reverseProxy.Rewrite = nil
		reverseProxy.ModifyResponse = nil
		reverseProxyPool.Put(reverseProxy)
	}()

	var begin time.Time
	reverseProxy.Rewrite = func(req *httputil.ProxyRequest) {
		begin = time.Now()
		req.Out.Header.Set("HOST", r.Host)
		req.Out.Header.Del("Accept-Encoding")
	}

	// tr := http.DefaultTransport.(*http.Transport)
	// if p.proxy != nil {
	// 	tr.Proxy = func(_ *http.Request) (*url.URL, error) {
	// 		return p.proxy, nil
	// 	}
	// }
	// tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}

	// reverseProxy.Transport = tr
	reverseProxy.Transport = HttpTransport()

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

		respBody := new(bytes.Buffer)

		resp.Body = io.NopCloser(io.TeeReader(resp.Body, respBody))

		responseData, err := io.ReadAll(resp.Body)
		if err != nil {
			fmt.Printf("io.ReadAll(resp.Body) error(%v)\n", err)
			return err
		}

		// 拦截修改数据
		if hookFunc != nil {
			responseData = hookFunc(r.URL, responseData)
		}

		size = int64(len(responseData))

		if len(responseData) > 0 {
			// gzip 补充
			if bGZIP {
				resp.Header.Set("Content-Encoding", "gzip")
				responseData = withGZIP(responseData)
			}
			// 重新计算 Content-Length
			resp.Header.Set("Content-Length", strconv.Itoa(len(responseData)))
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

func isSSERequest(r *http.Request) bool {
	contentType := r.Header.Get("Content-Type")
	return strings.Contains(strings.ToLower(contentType), "text/event-stream") ||
		strings.Contains(strings.ToLower(contentType), "application/x-ndjson") ||
		strings.Contains(strings.ToLower(contentType), "multipart/x-mixed-replace")
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

	for _, v := range p.exclude {
		if matched, _ := filepath.Match(v, host); matched {
			p.forward(w, r)
			return
		}
	}

	include := true
	for _, v := range p.include {
		include = false
		if matched, _ := filepath.Match(v, host); matched {
			include = true
			break
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

	if isSSERequest(r) {
		p.handleSSE(w, r)
		return
	}

	if r.URL.Host == "" {
		r.URL.Host = r.Host
	}

	if r.URL.Scheme == "" {
		r.URL.Scheme = "https"
	}

	for _, v := range p.replace {

		if ok, _ := filepath.Match(v[0], r.URL.String()); !ok {
			continue
		}

		switch v[1] {
		case "http://", "https://":
			if r, err := http.NewRequest(http.MethodGet, v[2], nil); err == nil {
				p.doReplace1(w, r)
			}
		case "file://":
			if data, err := os.ReadFile("/" + v[2]); err == nil {
				p.doReplace2(w, data)
			}
		}
		return
	}
	p.doRequest(w, r)
}
