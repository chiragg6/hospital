.PHONY: test build

test:
	go test ./...

build:
	mkdir -p bin
	go build -o bin/hospital ./cmd/hospital
	go build -o bin/surgeon ./cmd/surgeon
