IMG ?= ghcr.io/codespacesh/codewire-operator:latest
RELAY_IMG ?= ghcr.io/codespacesh/codewire:latest

.PHONY: build test lint docker-build docker-push install-crd run

build:
	go build -o bin/operator .

test:
	go test ./... -timeout 120s -count=1

lint:
	go vet ./...

docker-build:
	docker build -t $(IMG) .

docker-push: docker-build
	docker push $(IMG)

run:
	go run . --relay-image=$(RELAY_IMG)

install-crd:
	kubectl apply -f config/crd/

generate:
	controller-gen object paths="./api/..."
	controller-gen crd paths="./api/..." output:crd:dir=config/crd

manifests: generate
