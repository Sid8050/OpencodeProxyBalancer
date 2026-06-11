#!/bin/bash
# Installs the OpenCode Proxy as a macOS LaunchAgent
# Runs on login and stays alive in the background

LAUNCH_DIR="$HOME/Library/LaunchAgents"
PLIST="com.opencode.proxy.plist"

mkdir -p "$LAUNCH_DIR"

# Stop if already running
launchctl unload "$LAUNCH_DIR/$PLIST" 2>/dev/null

# Copy plist
cp "$(dirname "$0")/$PLIST" "$LAUNCH_DIR/$PLIST"

# Load it
launchctl load "$LAUNCH_DIR/$PLIST"

echo "✓ Proxy installed as background service"
echo "  Starts automatically on login"
echo "  Logs: $(dirname "$0")/proxy.log"
echo "  Dashboard: http://localhost:8320/dashboard"
echo ""
echo "To stop:  launchctl unload ~/Library/LaunchAgents/$PLIST"
echo "To start: launchctl load ~/Library/LaunchAgents/$PLIST"
