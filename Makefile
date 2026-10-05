.PHONY: test fmt fmt-check build build-ee build-agent up
test: fmt-check
	go vet ./... && go test ./...
fmt:
	gofmt -w .
fmt-check:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "not gofmt-formatted:"; echo "$$out"; exit 1; fi
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
