#!/bin/sh
set -e
cd "$(dirname "$0")"
mkdir -p classes
javac -d classes HttpDemo.java
echo "built: $(pwd)/classes"
