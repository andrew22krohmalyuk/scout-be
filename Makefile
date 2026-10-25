.PHONY: build build-server build-worker run run-worker test lint clean dev

build: build-server build-worker

build-server:
	go build -o bin/server ./cmd/server

build-worker:
	go build -o bin/worker ./cmd/worker

run: build-server
	./bin/server

run-worker: build-worker
	./bin/worker

test:
	go test ./... -race -cover

lint:
	go vet ./...

dev: 
	go tool air

clean:
	rm -rf bin/
