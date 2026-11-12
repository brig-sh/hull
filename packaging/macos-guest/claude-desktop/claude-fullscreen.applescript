-- Opens Claude and puts its window into full screen, so the guest display
-- shows the application and nothing else. Run by a LaunchAgent at login.
do shell script "/usr/bin/open -a Claude"
repeat 90 times
	delay 1
	try
		tell application "System Events"
			if exists process "Claude" then
				tell process "Claude"
					if (count of windows) > 0 then
						if value of attribute "AXFullScreen" of window 1 is true then return
						set frontmost to true
						set value of attribute "AXFullScreen" of window 1 to true
					else
						do shell script "/usr/bin/open -a Claude"
					end if
				end tell
			end if
		end tell
	end try
end repeat
