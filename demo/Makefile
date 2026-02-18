.PHONY: demo-image broker-image broker-build clean

DEMO_IMAGE ?= ghcr.io/codespacesh/codewire-demo
BROKER_IMAGE ?= ghcr.io/codespacesh/codewire-demo-broker
TAG ?= latest

# Build demo container image
demo-image:
	cd .. && go build -o demo/cw ./cmd/cw
	docker build -t $(DEMO_IMAGE):$(TAG) .
	rm -f cw

# Build broker binary locally
broker-build:
	cd broker && go build -o ../../demo-broker .

# Build broker container image
broker-image:
	docker build -t $(BROKER_IMAGE):$(TAG) broker/

clean:
	rm -f cw demo-broker
