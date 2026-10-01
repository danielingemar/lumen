.PHONY: test build build-ee build-agent up
test:
	go vet ./... && go test ./...
build:
	go build -o bin/lumen ./cmd/lumen
	go build -o bin/lumen-agent ./cmd/lumen-agent
build-agent:
	go build -o bin/lumen-agent ./cmd/lumen-agent
build-ee:
	go build -tags enterprise -o bin/lumen-ee ./cmd/lumen
up:
	docker compose -f deploy/docker-compose.yml up --build

.PHONY: dev
dev:
	./dev.sh
