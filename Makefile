BINARY  := prreview
VERSION := 0.1.0

.PHONY: build install clean release

build:
	go build -o bin/$(BINARY) .

install: build
	cp bin/$(BINARY) /usr/local/bin/$(BINARY)

release:
	GOOS=darwin GOARCH=arm64 go build -ldflags "-s -w" -o bin/$(BINARY)-darwin-arm64 .
	GOOS=darwin GOARCH=amd64 go build -ldflags "-s -w" -o bin/$(BINARY)-darwin-amd64 .

clean:
	rm -rf bin
