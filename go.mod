module github.com/waltu/shining-light

go 1.24.1

require (
	// External dependencies will be listed here by go mod tidy
	github.com/go-redis/redis/v8 v8.11.5
	github.com/gorilla/mux v1.8.1
	github.com/gorilla/websocket v1.5.3
	google.golang.org/grpc v1.71.1
	google.golang.org/protobuf v1.36.6
)

// Indirect dependencies (if any) will be added below by go mod tidy

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
	golang.org/x/net v0.34.0 // indirect
	golang.org/x/sys v0.29.0 // indirect
	golang.org/x/text v0.21.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20250115164207-1a7da9e5054f // indirect
)

replace github.com/waltu/shining-light/proto/chargercommander => ./proto/chargercommander

replace github.com/waltu/shining-light/proto/ocppforwarder => ./proto/ocppforwarder
