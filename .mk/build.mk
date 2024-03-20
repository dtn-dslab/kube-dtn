CNI_IMG := g-lbjv2952-docker.pkg.coding.net/dtn/kubedtn/kubedtn
GOPATH ?= ${HOME}/go
ARCHS := "linux/amd64"

## Build CNI plugin and daemon
cni-build:
	CGO_ENABLED=1 GOOS=linux go build -o bin/kubedtn github.com/y-young/kube-dtn/plugin
	CGO_ENABLED=1 GOOS=linux go build -o bin/kubedtnd github.com/y-young/kube-dtn/daemon

.PHONY: cni-docker
## Build CNI plugin docker image
cni-docker:
	@echo 'Creating docker image ${CNI_IMG}:${COMMIT}'
	docker buildx create --use --name=multiarch --driver-opt network=host --buildkitd-flags '--allow-insecure-entitlement network.host' --node multiarch && \
	docker buildx build --load \
	--build-arg LDFLAGS=${LDFLAGS} \
	--platform "${ARCHS}" \
	--tag ${CNI_IMG}:${COMMIT} \
	-f docker/Dockerfile.cni \
	.

cni-push:
	docker push ${CNI_IMG}:${COMMIT}

## Build CLI
cmd-build:
	CGO_ENABLED=1 GOOS=linux go build -o bin/kubedtn-cli github.com/y-young/kube-dtn/cmd
