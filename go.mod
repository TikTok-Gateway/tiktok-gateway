module tiktok-chat-bot

go 1.25.8

require (
	github.com/google/uuid v1.6.0
	github.com/httpjamesm/matrix-tiktok v0.0.0
	github.com/rs/zerolog v1.35.0
	google.golang.org/protobuf v1.36.11
	modernc.org/sqlite v1.39.1
)

require (
	github.com/coder/websocket v1.8.14 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/go-resty/resty/v2 v2.17.2 // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v0.1.9 // indirect
	github.com/pierrec/lz4/v4 v4.1.26 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/exp v0.0.0-20260312153236-7ab1446f8b90 // indirect
	golang.org/x/net v0.53.0 // indirect
	golang.org/x/sys v0.43.0 // indirect
	modernc.org/libc v1.66.10 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)

replace github.com/httpjamesm/matrix-tiktok => ./third_party/matrix-tiktok
