PREFIX ?= /usr/local

.PHONY: build test install

build:
	mkdir -p bin
	go build -o bin/cryobank .
	ln -sf cryobank bin/git-freeze
	ln -sf cryobank bin/git-thaw

test:
	go test -race ./...

install: build
	install -d $(PREFIX)/bin
	install bin/cryobank $(PREFIX)/bin/cryobank
	ln -sf cryobank $(PREFIX)/bin/git-freeze
	ln -sf cryobank $(PREFIX)/bin/git-thaw
