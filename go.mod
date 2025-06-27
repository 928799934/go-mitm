module github.com/928799934/go-mitm

go 1.24

toolchain go1.24.3

require (
	github.com/andybalholm/brotli v1.0.6
	github.com/gorilla/websocket v1.5.3
	github.com/refraction-networking/utls v1.7.3
	golang.org/x/net v0.41.0
)

require (
	github.com/cloudflare/circl v1.5.0 // indirect
	github.com/klauspost/compress v1.17.4 // indirect
	golang.org/x/crypto v0.39.0 // indirect
	golang.org/x/sys v0.33.0 // indirect
	golang.org/x/text v0.26.0 // indirect
)

// replace github.com/gorilla/websocket v1.5.3 => ../websocket
