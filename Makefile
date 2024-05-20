all: lint test build

test: lint
	go test -timeout=10s -cover -race -bench=. -benchmem ./...

build:
	# static build for alpine
	GOOS=linux GOARCH=amd64 GOAMD64=v3 CGO_ENABLED=0 go build -ldflags="-w -s" ./...

lint: bin/golangci-lint
	go fmt ./...
	go vet ./...
	bin/golangci-lint -c .golangci.yml run ./...
	go mod tidy

bin/golangci-lint: bin
	GOBIN=$(PWD)/bin go install github.com/golangci/golangci-lint/cmd/golangci-lint@v1.55.2

setup: bin/golangci-lint
	go mod download

image-build:
	docker build -t kostal2influx .
