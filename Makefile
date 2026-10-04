.PHONY: build run test check release clean

build:
	go build -trimpath -o bin/dublift ./cmd/dublift

run: build
	./bin/dublift

test:
	go test -race ./...

check:
	go vet ./...
	go mod verify

release:
	./scripts/build.sh

clean:
	rm -f bin/dublift bin/dublift-linux-amd64 bin/dublift-linux-arm64
