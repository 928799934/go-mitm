package proxy

import (
	"bytes"
	"compress/gzip"
	"strings"
	"sync"
)

// pool 压缩池
var pool = &sync.Pool{
	New: func() interface{} {
		return bytes.NewBuffer([]byte{})
	},
}

// dealWithGZIP 处理gzip
func dealWithGZIP(headers []string) ([]string, bool) {
	var (
		newHeaders []string
		bGZIP      bool
	)
	for _, v := range headers {
		if strings.ToLower(strings.Trim(v, " ")) == "gzip" {
			bGZIP = true
			continue
		}
		newHeaders = append(newHeaders, v)
	}
	return newHeaders, bGZIP
}

// withGZIP 压缩数据
func withGZIP(data []byte) []byte {
	b := pool.Get().(*bytes.Buffer)
	defer func() {
		b.Reset()
		pool.Put(b)
	}()

	w := gzip.NewWriter(b)
	// w, _ := gzip.NewWriterLevel(b, gzip.DefaultCompression)
	_, _ = w.Write(data)
	_ = w.Close()
	return b.Bytes()
}
