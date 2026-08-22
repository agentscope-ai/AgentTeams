.PHONY: build build-controller build-cli clean

build: build-controller build-cli

build-controller:
	CGO_ENABLED=1 go build -o bin/hiclaw-controller ./cmd/controller/

build-cli:
	CGO_ENABLED=0 go build -o bin/hiclaw ./cmd/hiclaw/

clean:
	rm -rf bin/

test:
	go test ./...
