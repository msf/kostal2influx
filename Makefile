all: lint test build

test:
	go mod tidy
	go test -timeout=10s -race -benchmem ./...

build:
	go build ./...

lint: bin/golangci-lint
	go fmt ./...
	go vet ./...
	bin/golangci-lint -c .golangci.yml run ./...

bin/golangci-lint:
	wget -O- -nv https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s v1.34.1

setup: bin/golangci-lint
	go mod download

image-build:
	docker build -t sidecar .

image-push: image-build
	docker image tag sidecar:latest localhost:32000/sidecar
	docker push localhost:32000/sidecar

deploy: image-push
	kubectl rollout restart deployment sender
	kubectl rollout restart deployment pt-en
	kubectl rollout restart deployment en-pt
	kubectl rollout restart deployment en-es

latency-test-web:
	hey -c 2 -z 80s http://10.152.183.73:8080/web
	hey -c 1 -z 80s http://10.152.183.73:8080/web

latency-test-queue:
	hey -c 2 -z 80s http://10.152.183.73:8080/queue
	sleep 40
	hey -c 1 -z 80s http://10.152.183.73:8080/queue
