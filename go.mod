module github.com/BananaLabs-OSS/Pulp-ext-pty

go 1.25.6

require (
	github.com/BananaLabs-OSS/Pulp v0.0.0
	github.com/aymanbagabas/go-pty v0.2.3
	github.com/tetratelabs/wazero v1.11.0
	github.com/vmihailenco/msgpack/v5 v5.4.1
)

require (
	github.com/creack/pty v1.1.24 // indirect
	github.com/u-root/u-root v0.16.0 // indirect
	github.com/vmihailenco/tagparser/v2 v2.0.0 // indirect
	golang.org/x/crypto v0.51.0 // indirect
	golang.org/x/sys v0.44.0 // indirect
)

replace github.com/BananaLabs-OSS/Pulp => ../Pulp
