package proxy

import (
	"bufio"
	"bytes"
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

	reqBody := new(bytes.Buffer)

	r.Body = io.NopCloser(io.TeeReader(r.Body, reqBody))
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

	if origin := req.Header.Get("Origin"); origin != "" {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Credentials", "true")
	}

	// 将上游 SSE 数据流转发给客户端
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	// 发送请求到目标服务器
	client := &http.Client{Transport: HttpTransport()}
	resp, err := client.Do(req)
	if err != nil {
		_, _ = w.Write([]byte("event: error\ndata: " + err.Error() + "\n\n"))
		flusher.Flush()
		return
	}
	defer resp.Body.Close()

	var msg *Message
	// 记录SSE连接信息
	if p.messageChan != nil {
		msg = &Message{
			Url:          r.URL.String(),
			RemoteAddr:   r.RemoteAddr,
			Method:       r.Method,
			ReqBody:      reqBody.String(),
			Type:         "text/event-stream",
			Status:       uint16(resp.StatusCode),
			ReqHeader:    map[string]string{"Accept": r.Header.Get("Accept")},
			RespBodyChan: make(chan []byte, 10240),
		}

		defer close(msg.RespBodyChan)

		p.messageChan <- msg
	}

	// 从目标服务器读取SSE事件并按行转发到客户端
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		// 获取一行数据
		data := scanner.Bytes()

		// 追加换行符
		data = append(data, '\n')

		// 发送数据到客户端
		if _, err = w.Write(data); err != nil {
			break
		}
		flusher.Flush()

		if msg != nil {
			msg.RespBodyChan <- data
		}
	}

	// 处理扫描过程中的错误
	if err := scanner.Err(); err != nil && err != io.EOF {
		_, _ = w.Write([]byte("event: error\ndata: Connection closed\n\n"))
		flusher.Flush()
	}
}
