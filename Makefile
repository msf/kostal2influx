all: lint test build

test:
	go mod tidy
	go test -timeout=10s -race -benchmem ./...

build:
	# static build for alpine
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-w -s" ./...

lint: bin/golangci-lint
	go fmt ./...
	go vet ./...
	bin/golangci-lint -c .golangci.yml run ./...

bin/golangci-lint:
	wget -O- -nv https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s v1.34.1

setup: bin/golangci-lint
	go mod download

image-build:
	docker build -t kostal2influx .

