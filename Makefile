.PHONY: build-client build test docker-client clean release-client debug-up debug-down debug-logs debug-test

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

debug-up:
	docker compose -f docker-compose.debug.yml up -d --build

debug-down:
	docker compose -f docker-compose.debug.yml down -v

debug-logs:
	docker compose -f docker-compose.debug.yml logs -f --tail=200

debug-test: ## Run e2e test (auto-clones/pulls server source unless PLEXTUNNEL_SERVER_IMAGE is set)
	./scripts/e2e-debug.sh
