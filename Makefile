IMAGE   ?= ghcr.io/msf/kostal2influx
VERSION ?= $(shell git describe --tags --always --dirty)

all: lint test build

test: lint
	go test -timeout=10s -cover -race -bench=. -benchmem ./...

build:
	# static daemon binary for the container
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-w -s" -o kostal2influx .

# one-shot InfluxDB -> VictoriaMetrics history importer
backfill: bin
	go build -o bin/backfill ./cmd/backfill

lint: bin/golangci-lint
	go fmt ./...
	go vet ./...
	bin/golangci-lint -c .golangci.yml run ./...
	go mod tidy

bin/golangci-lint: bin
	GOBIN=$(PWD)/bin go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2

bin:
	mkdir -p bin

setup: bin/golangci-lint
	go mod download

image-build:
	docker build -t $(IMAGE):$(VERSION) -t $(IMAGE):latest .

image-push: image-build
	docker push $(IMAGE):$(VERSION)
	docker push $(IMAGE):latest
