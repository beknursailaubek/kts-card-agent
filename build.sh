#!/usr/bin/env bash
# Сборка exe для Windows со сведениями о программе: dist/kts-card-agent.exe и dist/kts-card-agent.json
set -euo pipefail
cd "$(dirname "$0")"
v=$(cat VERSION)
[[ $v =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "VERSION '$v' не вида X.Y.Z" >&2; exit 1; }
rm -rf dist && mkdir dist
(cd cmd/kts-card-agent && go run github.com/tc-hib/go-winres@v0.3.3 make --in winres/winres.json --arch amd64 --product-version "$v" --file-version "$v")
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-H=windowsgui -X main.version=$v" -o dist/kts-card-agent.exe ./cmd/kts-card-agent
printf '{"version":"%s"}\n' "$v" > dist/kts-card-agent.json
