#!/usr/bin/env bash
set -e
if [[ ${DEBUG} -gt 0 ]]; then set -x; fi

# Run a command in a private network namespace
# set up by CNI plugins
contid=$(printf '%x%x%x%x' $RANDOM $RANDOM $RANDOM $RANDOM)
netnspath=/var/run/netns/$contid

ip netns add $contid
CNI_PATH=/opt/cni/bin ./exec-plugins.sh add $contid $netnspath

echo now run user command 
ip netns exec $contid "$@"

echo press enter to clean
read

function cleanup() {
	CNI_PATH=/opt/cni/bin ./exec-plugins.sh del $contid $netnspath
	ip netns delete $contid
}
trap cleanup EXIT
