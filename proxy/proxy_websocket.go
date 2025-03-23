package proxy

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var (
	dialer = &websocket.Dialer{
		Proxy:            http.ProxyFromEnvironment,
		HandshakeTimeout: 45 * time.Second,
		// EnableCompression: true,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		Jar:             cookieJar(),
	}
	// 添加 WebSocket upgrader
	wsUpgrader = websocket.Upgrader{
		// EnableCompression: true,
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}
)

func cookieJar() http.CookieJar {
	jar, _ := cookiejar.New(nil)
	return jar
}

type WebSocket websocket.Dialer

func (ws *WebSocket) Dialer() *websocket.Dialer {
	return (*websocket.Dialer)(ws)
}

func (ws *WebSocket) Clone() *WebSocket {
	return &WebSocket{
		Proxy:             ws.Proxy,
		HandshakeTimeout:  ws.HandshakeTimeout,
		EnableCompression: ws.EnableCompression,
		TLSClientConfig:   ws.TLSClientConfig,
		NetDialContext:    ws.NetDialContext,
		NetDial:           ws.NetDial,
		NetDialTLSContext: ws.NetDialTLSContext,
		ReadBufferSize:    ws.ReadBufferSize,
		WriteBufferSize:   ws.WriteBufferSize,
		WriteBufferPool:   ws.WriteBufferPool,
		Subprotocols:      ws.Subprotocols,
		Jar:               ws.Jar,
	}
}

func WebSocketDialer() *WebSocket {
	if socks5Func != nil && dialer.NetDialContext == nil {
		dialer.NetDialContext = socks5Func
	}

	if socks5Func == nil && dialer.NetDialContext != nil {
		dialer.NetDialContext = nil
	}

	if proxyFunc != nil && dialer.Proxy == nil {
		dialer.Proxy = proxyFunc
	}

	if proxyFunc == nil && dialer.Proxy != nil {
		dialer.Proxy = nil
	}
	return (*WebSocket)(dialer)
}

// 新增：处理 WebSocket 连接
func (p *Proxy) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	// 复制原始请求头
	header := make(http.Header)
	for k, v := range r.Header {
		switch {
		case k == "Upgrade":
		case k == "Connection":
		case k == "Sec-Websocket-Key":
		case k == "Sec-Websocket-Version":
		case k == "Sec-Websocket-Extensions":
		default:
			header[k] = v
		}
	}

	// 创建到目标服务器的 WebSocket 连接
	targetURL := r.URL

	if targetURL.Scheme == "" {
		u, _ := url.Parse(r.Header.Get("origin"))
		targetURL.Scheme = u.Scheme
	}

	if targetURL.Host == "" {
		targetURL.Host = r.Host
	}

	if targetURL.Scheme == "http" {
		targetURL.Scheme = "ws"
	} else if targetURL.Scheme == "https" {
		targetURL.Scheme = "wss"
	}

	uri := fmt.Sprintf("%s://%s%s", targetURL.Scheme, targetURL.Host, targetURL.Path)

	if querys := targetURL.Query(); len(querys) > 0 {
		uri += "?" + querys.Encode()
	}

	// 连接目标 WebSocket 服务器
	dialer := WebSocketDialer().Dialer()
	// dialer := &websocket.Dialer{
	// 	// Proxy:             http.ProxyFromEnvironment,
	// 	HandshakeTimeout:  45 * time.Second,
	// 	EnableCompression: true,
	// 	TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
	// }

	// dialer.Proxy = func(_ *http.Request) (*url.URL, error) {
	// 	return p.proxy, nil
	// 	// return url.Parse("http://127.0.0.1:1080")
	// }

	targetConn, resp, err := dialer.Dial(uri, header)
	if err != nil {
		if resp != nil {
			copyHeader(w.Header(), resp.Header)
			w.WriteHeader(resp.StatusCode)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	defer targetConn.Close()

	upgradeHeader := http.Header{}
	upgradeHeader.Add("Sec-WebSocket-Protocol", r.Header.Get("Sec-WebSocket-Protocol"))

	for _, v := range dialer.Jar.Cookies(r.URL) {
		upgradeHeader.Add("Set-Cookie", v.String())
	}

	// 升级客户端连接为 WebSocket
	clientConn, err := wsUpgrader.Upgrade(w, r, upgradeHeader)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer clientConn.Close()

	// 记录 WebSocket 连接信息
	if p.messageChan != nil {
		p.messageChan <- &Message{
			Url:        r.URL.String(),
			RemoteAddr: r.RemoteAddr,
			Method:     r.Method,
			Type:       "websocket",
			Status:     101,
			ReqHeader: map[string]string{
				"Upgrade":    r.Header.Get("Upgrade"),
				"Connection": r.Header.Get("Connection"),
			},
		}
	}

	var g sync.WaitGroup
	g.Add(2)
	go func() {
		defer g.Done()
		p.proxyWebSocket(clientConn, targetConn, nil)
	}()

	go func() {
		defer g.Done()
		p.proxyWebSocket(targetConn, clientConn, r.URL)
	}()
	g.Wait()
}

// 新增：转发 WebSocket 消息
func (p *Proxy) proxyWebSocket(dst, src *websocket.Conn, url *url.URL) error {

	for {
		messageType, message, err := src.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseGoingAway) {
				return nil
			}
			return err
		}

		if hookFunc != nil && url != nil {
			message = hookFunc(url, message)
		}

		err = dst.WriteMessage(messageType, message)
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseGoingAway) {
				return nil
			}
			return err
		}

		// 记录 WebSocket 消息
		// if p.messageChan != nil {
		// 	p.messageChan <- &Message{
		// 		Type:     "websocket_message",
		// 		ReqBody:  string(message),
		// 		RespBody: fmt.Sprintf("WebSocket message type: %d", messageType),
		// 	}
		// }
	}
}

// 新增：检查是否为 WebSocket 请求
func isWebSocketRequest(r *http.Request) bool {
	return strings.ToLower(r.Header.Get("Connection")) == "upgrade" &&
		strings.ToLower(r.Header.Get("Upgrade")) == "websocket"
}
