.PHONY: build release test clean

build:
	go build -buildvcs=false -trimpath -ldflags="-s -w" -o hftools ./cmd/hftools

release:
	./scripts/build-release.sh

test:
	go test ./...

clean:
	rm -f hftools
