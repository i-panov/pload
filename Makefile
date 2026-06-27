.PHONY: all build clean test vet lint run

BINARY   := pload
BIN_DIR  := bin

all: build

build:
	mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/$(BINARY) ./cmd/pload/

clean:
	rm -rf $(BIN_DIR)

test:
	go test -v -race -count=1 ./...

vet:
	go vet ./...

lint:
	golangci-lint run ./...

run: build
	./$(BIN_DIR)/$(BINARY)
