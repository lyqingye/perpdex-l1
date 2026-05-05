.PHONY: build run test proto tidy clean

BINARY ?= oracle-sidecar
OUT    ?= ./build/$(BINARY)

build:
	go build -trimpath -buildvcs=true -o $(OUT) ./cmd/oracle-sidecar

run:
	go run ./cmd/oracle-sidecar --config ./oracle.json -v

test:
	go test ./...

proto:
	protoc \
	  --go_out=service/proto --go_opt=paths=source_relative \
	  --go-grpc_out=service/proto --go-grpc_opt=paths=source_relative \
	  -I service/proto \
	  service/proto/oracle.proto

tidy:
	go mod tidy

clean:
	rm -rf build/
