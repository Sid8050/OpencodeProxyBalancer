set proxyDir to "/Users/sidharth/Documents/opencode-proxy/"
set proxyBin to proxyDir & "proxy"

try
	do shell script "pgrep -f " & quoted form of proxyBin
	set running to true
on error
	set running to false
end try

if running then
	set dlg to display dialog "✅ Proxy is running on port 8320" & return & "Dashboard: http://localhost:8320/dashboard" with title "OpenCode Proxy" buttons {"Stop", "OK"} default button "OK"
	if button returned of dlg is "Stop" then
		set confirm to display dialog "⚠️ Stopping disconnects OpenCode Go. Continue?" with title "OpenCode Proxy" buttons {"Cancel", "Stop"} default button "Cancel" with icon caution
		if button returned of confirm is "Stop" then
			do shell script "pkill -f " & quoted form of proxyBin
			display notification "Proxy stopped" with title "OpenCode Proxy"
		end if
	end if
else
	set dlg to display dialog "❌ Proxy is not running" with title "OpenCode Proxy" buttons {"Start", "OK"} default button "Start"
	if button returned of dlg is "Start" then
		do shell script "cd " & quoted form of proxyDir & " && nohup " & quoted form of proxyBin & " > proxy.log 2>&1 &"
		delay 1
		try
			do shell script "curl -s http://localhost:8320/health"
			display notification "Proxy started — port 8320" with title "OpenCode Proxy" sound name "Pop"
		on error
			display notification "Failed to start" with title "OpenCode Proxy"
		end try
	end if
end if
