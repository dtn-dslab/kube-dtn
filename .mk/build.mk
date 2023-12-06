CNI_IMG := registry.cn-hangzhou.aliyuncs.com/reins/kubedtn
GOPATH ?= ${HOME}/go
ARCHS := "linux/amd64"

## Build CNI plugin and daemon
cni-build:
	CGO_ENABLED=1 GOOS=linux go build -o bin/kubedtn ./plugin
	CGO_ENABLED=1 GOOS=linux go build -o bin/kubedtnd ./daemon

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
	CGO_ENABLED=1 GOOS=linux go build -o bin/kubedtn-cli ./cmd