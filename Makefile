.PHONY: build run vet test clean docker

BINARY=chimera
PKG=./...

build:
	go build -ldflags="-s -w" -o $(BINARY) ./cmd/chimera

run:
	go run ./cmd/chimera

vet:
	go vet $(PKG)

test:
	go test $(PKG) -v

clean:
	rm -f $(BINARY)
	rm -rf browser_data logs

docker:
	docker compose up --build -d

docker-logs:
	docker compose logs -f

install-rod:
	go run github.com/go-rod/rod/lib/launcher@latest

help:
	@echo "Targets: build run vet test clean docker docker-logs"

