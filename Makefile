.PHONY: build run test clean tidy docker

BINARY=gateway

build: tidy
	CGO_ENABLED=0 go build -ldflags="-s -w" -trimpath -o $(BINARY) .

run: build
	./$(BINARY)

test:
	go test -v -count=1 -race ./...

tidy:
	go mod tidy

clean:
	rm -f $(BINARY) gateway.db

docker:
	docker compose up --build -d

docker-stop:
	docker compose down

lint:
	go vet ./...
