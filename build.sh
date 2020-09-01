#!/bin/sh

gofmt -s -w ./src
go vet ./src
go test ./src

echo "Building linux binary:\n"
go build -a -v -gccgoflags="-static-libgo -static" -o goben-linux ./src
file goben-linux

echo "\n\nBuilding windows (amd64) binary:\n"
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -a -v -gccgoflags="-static-libgo -static" -o goben-amd64.exe ./src
file goben-amd64.exe


echo "Building windows (386) binary:"
GOOS=windows GOARCH=386 CGO_ENABLED=0 go build -a -v -gccgoflags="-static-libgo -static" -o goben-386.exe ./src
file goben-386.exe


