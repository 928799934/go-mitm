package proxy

import (
	"bufio"
	"io"
	"net/http"
)

func (p *Proxy) handleSSE(w http.ResponseWriter, r *http.Request) {
	if r.URL.Scheme == "" {
		r.URL.Scheme = "https"
	}

	if r.URL.Host == "" {
		r.URL.Host = r.Host
	}

	// 创建到目标服务器的请求
	req, err := http.NewRequest(r.Method, r.URL.String(), r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// 设置请求头
	req.Header = r.Header.Clone()

	// 设置SSE相关的响应头
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	// 使用http.Hijacker获取底层连接
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "Hijacking not supported", http.StatusInternalServerError)
		return
	}

	// 获取客户端连接
	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer clientConn.Close()

	// 写入SSE响应头
	if _, err = clientConn.Write([]byte("HTTP/1.1 200 OK\r\n")); err != nil {
		return
	}

	// 写入响应头
	if _, err = clientConn.Write([]byte("Content-Type: text/event-stream\r\n")); err != nil {
		return
	}

	if _, err = clientConn.Write([]byte("Cache-Control: no-cache\r\n")); err != nil {
		return
	}

	if _, err = clientConn.Write([]byte("Connection: keep-alive\r\n")); err != nil {
		return
	}

	if _, err = clientConn.Write([]byte("Access-Control-Allow-Origin: *\r\n\r\n")); err != nil {
		return
	}

	// 发送请求到目标服务器
	client := &http.Client{Transport: HttpTransport()}
	resp, err := client.Do(req)
	if err != nil {
		_, _ = clientConn.Write([]byte("event: error\ndata: " + err.Error() + "\n\n"))
		return
	}
	defer resp.Body.Close()

	// 记录SSE连接信息
	if p.messageChan != nil {
		p.messageChan <- &Message{
			Url:        r.URL.String(),
			RemoteAddr: r.RemoteAddr,
			Method:     r.Method,
			Type:       "text/event-stream",
			Status:     uint16(resp.StatusCode),
			ReqHeader:  map[string]string{"Accept": r.Header.Get("Accept")},
		}
	}

	// 从目标服务器读取SSE事件并按行转发到客户端
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		// 获取一行数据
		line := scanner.Bytes()

		// 处理数据（可以在这里添加数据修改逻辑）
		data := append(line, '\n')
		if hookFunc != nil {
			data = hookFunc(r.URL, data)
		}

		// 发送数据到客户端
		if _, err = clientConn.Write(data); err != nil {
			break
		}
	}

	// 处理扫描过程中的错误
	if err := scanner.Err(); err != nil && err != io.EOF {
		_, _ = clientConn.Write([]byte("event: error\ndata: Connection closed\n\n"))
	}
}
