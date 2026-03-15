.PHONY: all build run clean test

BINARY_NAME=vartop
BIN_DIR=bin

all: build

build:
	@mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/$(BINARY_NAME) main.go

run: build
	./$(BIN_DIR)/$(BINARY_NAME)

clean:
	rm -rf $(BIN_DIR)

test:
	go test ./...
