.PHONY: build run test lint clean

BINARY := bin/scout
CMD    := ./cmd/server

build:
	go build -o $(BINARY) $(CMD)

run: build
	./$(BINARY)

test:
	go test ./... -race -cover

lint:
	go vet ./...

clean:
	rm -rf bin/
