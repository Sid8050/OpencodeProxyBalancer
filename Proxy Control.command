#!/bin/bash
PROXY_DIR="/Users/sidharth/Documents/opencode-proxy"
PROXY_BIN="$PROXY_DIR/proxy"

clear
echo "═══════════════════════════════════"
echo "   OpenCode Proxy — Control Panel"
echo "═══════════════════════════════════"
echo ""

if pgrep -f "$PROXY_BIN" > /dev/null 2>&1; then
    echo "  Status: ✅ RUNNING  (port 8320)"
    echo ""
    echo "  1) Open Dashboard"
    echo "  2) Stop Proxy"
    echo ""
else
    echo "  Status: ❌ STOPPED"
    echo ""
    echo "  1) Start Proxy"
    echo ""
fi
echo "  0) Quit"
echo ""
read -p "  Choice: " choice

case $choice in
    1)
        if pgrep -f "$PROXY_BIN" > /dev/null 2>&1; then
            open "http://localhost:8320/dashboard"
            echo "  Opening dashboard..."
        else
            echo "  Starting proxy..."
            cd "$PROXY_DIR"
            nohup "$PROXY_BIN" > proxy.log 2>&1 &
            sleep 2
            if curl -s http://localhost:8320/health > /dev/null 2>&1; then
                echo "  ✅ Proxy started on port 8320"
                echo "  Dashboard: http://localhost:8320/dashboard"
            else
                echo "  ❌ Failed to start. Check proxy.log"
            fi
        fi
        sleep 2
        ;;
    2)
        if pgrep -f "$PROXY_BIN" > /dev/null 2>&1; then
            echo "  ⚠️  This will disconnect OpenCode Go."
            read -p "  Stop anyway? [y/N]: " confirm
            if [ "$confirm" = "y" ] || [ "$confirm" = "Y" ]; then
                pkill -f "$PROXY_BIN"
                echo "  Proxy stopped."
            fi
        fi
        sleep 2
        ;;
esac
