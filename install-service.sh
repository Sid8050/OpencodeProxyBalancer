#!/bin/bash
set -e

PROXY_DIR="$HOME/Documents/opencode-proxy"
LAUNCH_DIR="$HOME/Library/LaunchAgents"
PLIST_NAME="com.opencode.proxy.plist"

mkdir -p "$LAUNCH_DIR"

launchctl unload "$LAUNCH_DIR/$PLIST_NAME" 2>/dev/null || true

cat > "$LAUNCH_DIR/$PLIST_NAME" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.opencode.proxy</string>
    <key>ProgramArguments</key>
    <array>
        <string>$PROXY_DIR/proxy</string>
    </array>
    <key>WorkingDirectory</key>
    <string>$PROXY_DIR</string>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>$PROXY_DIR/proxy.log</string>
    <key>StandardErrorPath</key>
    <string>$PROXY_DIR/proxy.log</string>
    <key>ProcessType</key>
    <string>Background</string>
</dict>
</plist>
PLIST

launchctl load "$LAUNCH_DIR/$PLIST_NAME" 2>/dev/null || true

echo "✓ Proxy installed as background service"
echo "  Starts automatically on login"
echo "  Logs: $PROXY_DIR/proxy.log"
echo "  Dashboard: http://localhost:8320/dashboard"
echo ""
echo "To stop:  launchctl unload $LAUNCH_DIR/$PLIST_NAME"
echo "To start: launchctl load $LAUNCH_DIR/$PLIST_NAME"
echo ""
echo "⚠️  macOS Gatekeeper may block unsigned binaries."
echo "    If the service fails: codesign -s - -f $PROXY_DIR/proxy"
