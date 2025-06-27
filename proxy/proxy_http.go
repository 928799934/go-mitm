package proxy

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andybalholm/brotli"
)

var (
	tr = &http.Transport{
		// Proxy: http.ProxyFromEnvironment,

		// DialContext: (&net.Dialer{
		// 	Timeout:   30 * time.Second,
		// 	KeepAlive: 30 * time.Second,
		// }).DialContext,
		// ForceAttemptHTTP2:     true,

		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
		DisableCompression:    true,
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
		tr.DialTLSContext = socks5TLSFunc
	}
	if socks5Func == nil && tr.DialContext != nil {
		tr.DialContext = nil
		tr.DialTLSContext = nil
	}

	if proxyFunc != nil && tr.Proxy == nil {
		tr.Proxy = proxyFunc
	}
	if proxyFunc == nil && tr.Proxy != nil {
		tr.Proxy = nil
	}
	return tr

	// spec := &tlsutls.ClientHelloSpec{
	// 	TLSVersMin: tls.VersionTLS12,
	// 	TLSVersMax: tls.VersionTLS13,
	// 	CipherSuites: []uint16{
	// 		tls.TLS_AES_128_GCM_SHA256, // TLS 1.3
	// 		tls.TLS_AES_256_GCM_SHA384, // TLS 1.3
	// 		tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
	// 		tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
	// 	},
	// 	Extensions: []tlsutls.TLSExtension{
	// 		&tlsutls.SNIExtension{},
	// 		&tlsutls.SupportedCurvesExtension{Curves: []tlsutls.CurveID{tlsutls.GREASE_PLACEHOLDER, tlsutls.X25519, tlsutls.CurveP256}},
	// 		&tlsutls.SupportedPointsExtension{SupportedPoints: []byte{0}},
	// 		&tlsutls.SignatureAlgorithmsExtension{
	// 			SupportedSignatureAlgorithms: []tlsutls.SignatureScheme{
	// 				tlsutls.ECDSAWithP256AndSHA256,
	// 				tlsutls.PSSWithSHA256,
	// 				tlsutls.PKCS1WithSHA256,
	// 			},
	// 		},
	// 		&tlsutls.SupportedVersionsExtension{
	// 			Versions: []uint16{tls.VersionTLS13, tls.VersionTLS12},
	// 		},
	// 		&tlsutls.ALPNExtension{AlpnProtocols: []string{"h2", "http/1.1"}},
	// 		&tlsutls.StatusRequestExtension{},
	// 		&tlsutls.UtlsExtendedMasterSecretExtension{},
	// 	},
	// }

	// spec, _ := tlsutls.UTLSIdToSpec(tlsutls.HelloRandomized)
	// for i, ext := range spec.Extensions {
	// 	if _, ok := ext.(*tlsutls.ALPNExtension); ok {
	// 		spec.Extensions[i] = &tlsutls.ALPNExtension{AlpnProtocols: []string{"http/1.1"}}
	// 	}
	// }

	// return &UTLSTransport{
	// 	Socks5Addr: "127.0.0.1:10808",
	// 	Spec:       &spec,
	// 	Timeout:    10 * time.Second,
	// }
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

	// 识别 gzip
	acceptEncoding := strings.Split(r.Header.Get("Accept-Encoding"), ",")
	_, bGZIP := dealWithGZIP(acceptEncoding)

	if p.disableGZIP {
		bGZIP = false
	}

	// 获取 代理资源
	reverseProxy := reverseProxyPool.Get().(*httputil.ReverseProxy)
	defer func() {
		reverseProxy.Rewrite = nil
		reverseProxy.Director = nil
		reverseProxy.ModifyResponse = nil
		reverseProxyPool.Put(reverseProxy)
	}()

	var begin time.Time

	// reverseProxy.Director = func(r *http.Request) {
	// 	begin = time.Now()
	// 	r.Header.Set("HOST", r.Host)
	// 	r.Header.Del("Accept-Encoding")
	// 	r.Header.Set("Proxy-Connection", "close")
	// 	// var cookies []string
	// 	// for _, v := range r.Cookies() {
	// 	// 	cookies = append(cookies, v.Name+"="+v.Value)
	// 	// }
	// 	// sort.Strings(cookies)
	// 	// r.Header.Set("cookie", strings.Join(cookies, "; "))
	// }

	reverseProxy.Rewrite = func(req *httputil.ProxyRequest) {
		begin = time.Now()
		req.Out.Header.Set("HOST", r.Host)
		req.Out.Header.Del("Accept-Encoding")
		req.Out.Header.Set("Connection", "close")
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
		fmt.Printf("ErrorHandler url(%v) error(%v)\n", req.URL.String(), err)
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
	accept := r.Header.Get("Accept")
	contentType := r.Header.Get("Content-Type")
	return strings.Contains(strings.ToLower(contentType), "text/event-stream") ||
		strings.Contains(strings.ToLower(contentType), "application/x-ndjson") ||
		strings.Contains(strings.ToLower(contentType), "multipart/x-mixed-replace") ||
		strings.Contains(strings.ToLower(accept), "text/event-stream")
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

// 从 RemoteAddr 中提取 IP 地址
func getRealIP(req *http.Request) string {
	ip, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		return ""
	}
	if net.ParseIP(ip) == nil {
		return ""
	}
	return ip
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

func (p *Proxy) doRequestEx(rw http.ResponseWriter, r *http.Request) {

	//fmt.Println(strings.Repeat("#", 100))
	//fmt.Println("Request:")
	//requestDump, err := httputil.DumpRequest(r, true)
	//if err != nil {
	//	fmt.Println("Error dumping request:", err)
	//	return
	//}
	//fmt.Println(string(bytes.TrimSpace(requestDump)))

	reqBody, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(rw, "Failed to read request body", http.StatusInternalServerError)
		return
	}
	r.Body = io.NopCloser(bytes.NewBuffer(reqBody))

	//getBody, err := r.GetBody()
	//if err != nil {
	//	return
	//}
	//reqBody, err := io.ReadAll(getBody)

	// switch r.Method {
	// case http.MethodGet:
	// 	options := []string{
	// 		"--compressed", "--insecure",
	// 		"--socks5 127.0.0.1:10808",
	// 	}
	// 	curl.Http1Get(options, r.Header, r.URL.String())
	// case http.MethodPost:
	// }

	t := HttpTransport()
	begin := time.Now()
	fmt.Println(r.Header.Get("accept-encoding"))
	response, err := t.RoundTrip(r)
	spend := uint16(time.Now().Sub(begin).Milliseconds())
	if err != nil {
		http.Error(rw, err.Error(), http.StatusServiceUnavailable)
		return
	}

	defer func() {
		_ = response.Body.Close()
	}()

	copyHeader(rw.Header(), response.Header)
	rw.WriteHeader(response.StatusCode)

	//fmt.Println(strings.Repeat("#", 100))
	//fmt.Println("Response:")
	//responseDump, err := httputil.DumpResponse(response, true)
	//if err != nil {
	//	fmt.Println("Error dumping response:", err)
	//	return
	//}
	//fmt.Println(string(bytes.TrimSpace(responseDump)))

	var size int64
	var respBody string
	contentTypes := response.Header.Get("Content-Type")
	if strings.Contains(strings.ToLower(contentTypes), "image") || strings.Contains(strings.ToLower(contentTypes), "video") {
		size, _ = io.Copy(rw, response.Body)
	} else {
		bodyBytes, err := io.ReadAll(response.Body)
		if err != nil {
			http.Error(rw, "Failed to read response body", http.StatusInternalServerError)
			return
		}
		s, _ := rw.Write(bodyBytes)
		size = int64(s)

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
		// 拦截修改数据
		// if hookFunc != nil {
		// 	bodyBytes = hookFunc(r.URL, bodyBytes)
		// }

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
			RespTls:    getTLSInfo(response.TLS, r.TLS),
		}
	}(r, response)
}
