.PHONY: all clean-all build cleand-deps deps version

NAME="zbackup"
DATE := $(shell git log -1 --format="%cd" --date=short | sed s/-//g)
COUNT := $(shell git rev-list --count HEAD)
COMMIT := $(shell git rev-parse --short HEAD)

VERSION := "${DATE}.${COUNT}_${COMMIT}"

LDFLAGS := "-X main.version=${VERSION}"

default: all

all: clean-all deps build

version:
	@echo ${VERSION}

clean-all: clean-deps
	@echo clean builded binaries
	rm -rf .out/
	@echo done

build:
	@echo build
	go build -v -o .out/${NAME} -ldflags ${LDFLAGS}
	@echo done

clean-deps:
	@echo clean dependencies
	rm -rf vendor/*
	@echo done

deps:
	@echo fetch dependencies
	dep ensure -v
	@echo done
