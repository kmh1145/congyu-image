.PHONY: test build run docker

test:
	go test ./...
	go vet ./...

build:
	CGO_ENABLED=0 go build -trimpath -o congyu-image .

run:
	go run .

docker:
	docker build -t congyu-image:dev .
