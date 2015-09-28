MAKEFLAGS += --warn-undefined-variables
SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail
.DEFAULT_GOAL := build

ROOT := $(shell pwd)

clean:
	rm -f .build-builder
	rm -rf build/

# builds the build container, which contains everything we need
# to build and test cbc-pillowfight, but that we don't want to ship
# because the final image weighs in at close to 1GB
.build-builder:
	docker build -t="cbc-benchmark-builder" -f=Dockerfile-builder .
	@touch .build-builder

# grabs the binary and shared lib we need from the builder image
# and drops them into the much leaner production deployment image
build: .build-builder
	mkdir -p build
	docker cp $(shell docker create cbc-benchmark-builder):/libcouchbase/build/lib/libcouchbase.so.2.0.22 - > build/libcouchbase.so.2.0.22
	docker cp $(shell docker create cbc-benchmark-builder):/libcouchbase/build/bin/cbc-pillowfight - > build/cbc-pillowfight
	docker build -t="0x74696d/cbc-benchmark" -f=Dockerfile .

# push our image to the public registry
ship: build
	docker push 0x74696d/cbc-benchmark


# golang build

clean-go:
	docker rm cbc-build || true
	rm -rf .godeps .go-builder build-go

.go-builder:
	docker build -t=golang-alpine -f=Dockerfile-go-builder .
	@touch .go-builder

.godeps:
	mkdir -p .godeps/
	GOPATH=${ROOT}/.godeps:${ROOT} go get github.com/couchbase/gocb
	GOPATH=${ROOT}/.godeps:${ROOT} go get github.com/hashicorp/consul/api

# grabs the binary we need from builder image and drops
# them into the much leaner production deployment image
build-go: .go-builder .godeps
	mkdir -p build-go
	docker run -it --name cbc-build \
		-e CGO_ENABLED=0 \
		-e GOPATH=/cbc-benchmark/.godeps:/cbc-benchmark \
		-v ${ROOT}:/cbc-benchmark -w /cbc-benchmark \
		golang-alpine go build -a -o /cbc-benchmark/build-go/cbc-benchmark \
		&& docker wait cbc-build
	@docker kill cbc-build || true \
		&& docker rm cbc-build || true
	chmod +x ${ROOT}/build-go/cbc-benchmark
	cp ${ROOT}/Dockerfile-go ${ROOT}/build-go/
	cd ${ROOT}/build-go && docker build -t=0x74696d/cbc-benchmark-go -f=Dockerfile-go .

ship-go: build-go
	docker push 0x74696d/cbc-benchmark-go
