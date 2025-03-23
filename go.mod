module github.com/928799934/go-mitm

go 1.23.0

toolchain go1.23.2

require (
	github.com/andybalholm/brotli v1.0.5
	github.com/gorilla/websocket v1.5.3
	golang.org/x/net v0.37.0
)

// replace github.com/gorilla/websocket v1.5.3 => ../websocket
