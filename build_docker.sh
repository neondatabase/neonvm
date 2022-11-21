#!/bin/sh

set -ex

if [ -z "$CONTROLLER_IMG" ]; then
    echo "missing CONTROLLER_IMG env var"
    exit 1
elif [ -z "$RUNNER_IMG" ]; then
    echo "missing RUNNER_IMG env var"
    exit 1
fi

if [ ! -e hack/vmlinuz ]; then
    make kernel
fi

docker build -t "$CONTROLLER_IMG" .
docker push "$CONTROLLER_IMG"
docker build -t "$RUNNER_IMG" -f runner/Dockerfile .
docker push "$RUNNER_IMG"

make kustomize
KUSTOMIZE=$(pwd)/bin/kustomize

deployts=$(date +%s)
(
    cd config/controller
    $KUSTOMIZE edit set image controller=$CONTROLLER_IMG
    $KUSTOMIZE edit add annotation redeploy-at:$deployts --force
)
$KUSTOMIZE build config/default > neonvm.yaml
