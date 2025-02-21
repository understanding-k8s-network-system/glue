#!/bin/sh

tagPrefix=registry.local.io
ver=$1

if [[ "$ver" == "" ]]; then
	ver=latest
fi

echo "Usage: $0 [dockerRepo] [tag]"

docker rmi glue:$ver

cp ../bin/* .
strip glue glued

docker build -t glue:$ver .

rm -f glue glued

if [[ "$tagPrefix" != "" ]]; then
	docker tag glue:$ver $tagPrefix/glue:$ver
	docker push $tagPrefix/glue:$ver
fi

#echo "Compress image..."
#gzip glue-$ver.tar

echo "Done"
