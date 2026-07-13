.PHONY: build release test clean

build:
	go build -buildvcs=false -trimpath -ldflags="-s -w" -o hfdown ./cmd/hfdown

release:
	./scripts/build-release.sh

test:
	go test ./...

clean:
	rm -f hfdown
