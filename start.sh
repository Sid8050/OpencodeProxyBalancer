#!/bin/bash
cd "$(dirname "$0")"
if [ ! -f keys.json ]; then
  echo "❌ keys.json not found. Copy keys.json.template and add your API keys."
  exit 1
fi
mkdir -p bin
go build -o bin/proxy-server main.go && echo "✓ Built"
bin/proxy-server
