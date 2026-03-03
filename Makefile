.PHONY: build-client build test docker-client clean release-client

build-client:
	go build -o bin/plextunnel-client ./cmd/client

build: build-client

test:
	go test ./...

docker-client:
	docker build -f Dockerfile.client -t plextunnel/client:latest .

clean:
	rm -rf bin/

release-client:
	GOOS=linux GOARCH=amd64 go build -o bin/plextunnel-client-linux-amd64 ./cmd/client
	GOOS=linux GOARCH=arm64 go build -o bin/plextunnel-client-linux-arm64 ./cmd/client
	GOOS=darwin GOARCH=amd64 go build -o bin/plextunnel-client-darwin-amd64 ./cmd/client
	GOOS=darwin GOARCH=arm64 go build -o bin/plextunnel-client-darwin-arm64 ./cmd/client
	GOOS=windows GOARCH=amd64 go build -o bin/plextunnel-client-windows-amd64.exe ./cmd/client
