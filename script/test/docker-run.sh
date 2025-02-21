#!/usr/bin/env bash

# Run a docker container with network namespace set up by the
# CNI plugins.

# Example usage: ./docker-run.sh --rm busybox /sbin/ifconfig


contid=$(docker run -d --net=none k8s.gcr.io/pause:3.4.1 /bin/sleep 10000000)

echo create init container: $contid

pid=$(docker inspect -f '{{ .State.Pid }}' $contid)
netnspath=/proc/$pid/ns/net

echo init container pid = $pid
echo init container netnspath = $netnspath

echo "call CNI plugin...."

CNI_PATH=/opt/cni/bin ./exec-plugins.sh add $contid $netnspath

realcontid=$(docker run --net=container:$contid $@)
echo create user container: $realcontid

echo -----------------------
echo -n "press enter to delete test container..."
read

function cleanup() {
	CNI_PATH=/opt/cni/bin ./exec-plugins.sh del $contid $netnspath
	docker rm -f $contid >/dev/null
	docker rm -f $realcontid >/dev/null
}
trap cleanup EXIT