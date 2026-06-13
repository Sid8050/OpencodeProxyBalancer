#!/bin/bash
cd "$(dirname "$0")"
if [ ! -f keys.json ]; then
  echo "❌ keys.json not found. Create it with your API keys:"
  echo '  echo '"'"'{"keys":[{"name":"my-key","key":"sk-..."}],"usage":{}}'"'"' > keys.json'
  exit 1
fi
echo "→ Building..."
go build -o proxy . && echo "✓ Built"
echo "→ Starting on http://localhost:8320"
echo "  Dashboard: http://localhost:8320/dashboard"
./proxy
