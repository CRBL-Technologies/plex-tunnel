.PHONY: build-agent build-relay build test docker-agent docker-relay clean release-agent

build-agent:
	go build -o bin/plextunnel-agent ./cmd/agent

build-relay:
	go build -o bin/plextunnel-relay ./cmd/relay

build: build-agent build-relay

test:
	go test ./...

docker-agent:
	docker build -f Dockerfile.agent -t plextunnel/agent:latest .

docker-relay:
	docker build -f Dockerfile.relay -t plextunnel/relay:latest .

clean:
	rm -rf bin/

release-agent:
	GOOS=linux GOARCH=amd64 go build -o bin/plextunnel-agent-linux-amd64 ./cmd/agent
	GOOS=linux GOARCH=arm64 go build -o bin/plextunnel-agent-linux-arm64 ./cmd/agent
	GOOS=darwin GOARCH=amd64 go build -o bin/plextunnel-agent-darwin-amd64 ./cmd/agent
	GOOS=darwin GOARCH=arm64 go build -o bin/plextunnel-agent-darwin-arm64 ./cmd/agent
	GOOS=windows GOARCH=amd64 go build -o bin/plextunnel-agent-windows-amd64.exe ./cmd/agent
