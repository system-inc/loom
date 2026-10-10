# wsl-forward.ps1: the box's Windows host runs this at every start (the "WSL SSH forward" scheduled task,
# docs/windows-host.md). It starts the box's WSL, reads the address Windows gave it (a new one after every WSL start),
# and points this host's forwards at it: ssh's 2222 always, and on a host that serves the house cache, its 7380
# forward and the cache's own listen address inside WSL. One line per action goes to C:\wsl-forward.log, with netsh's
# and the install's own words. It never ends with ssh's forward unconfirmed: it waits for WSL's address and retries
# each forward until netsh takes it, however long that is, since ssh is the only way back into the box.
$distribution = 'Ubuntu-24.04'
$log = 'C:\wsl-forward.log'
function Say($message) { Add-Content -Path $log -Value ('{0:yyyy-MM-ddTHH:mm:ssK} {1}' -f (Get-Date), $message) }

# Forward <listen>:<port> to WSL's <ip>:<connect>, until netsh says it took it. An add over an existing listen address
# and port replaces that entry, so nothing is deleted first and the old forward stands until the new one is in; should
# a netsh refuse the add over an existing entry, set (which changes one) is asked in the same attempt.
function Forward($name, $listen, $port, $connect, $ip) {
	$attempt = 0
	while ($true) {
		$attempt++
		foreach ($verb in 'add', 'set') {
			$said = netsh interface portproxy $verb v4tov4 listenaddress=$listen listenport=$port connectaddress=$ip connectport=$connect 2>&1
			$status = $LASTEXITCODE
			$words = ("$said" -replace '\s+', ' ').Trim()
			if ($status -eq 0) {
				Say "${name}: ${listen}:${port} -> ${ip}:${connect} (netsh ${verb}: '$words')"
				return
			}
			Say "${name}: netsh $verb refused ${listen}:${port} -> ${ip}:${connect}, attempt $attempt, exit ${status}: '$words'"
		}
		Start-Sleep 10
	}
}

# WSL's address is eth0's own IPv4 address: hostname -I lists every address, in no promised order. WSL may take a while
# after Windows starts, and without its address no forward can be pointed, so this waits as long as it takes, saying so
# once a minute, and leaves the last boot's forwards standing meanwhile.
Start-Process wsl -ArgumentList "-d $distribution -- sleep infinity" -WindowStyle Hidden
$ip = $null
$waited = 0
while (-not $ip) {
	$shown = wsl -d $distribution --cd / -u root -- ip -4 -o address show dev eth0 scope global 2>&1
	if ("$shown" -match 'inet (\d+\.\d+\.\d+\.\d+)/') { $ip = $Matches[1]; break }
	if ($waited % 60 -eq 0) { Say "WSL $distribution has no eth0 address after $waited s ('$("$shown".Trim())'); still waiting" }
	Start-Sleep 5
	$waited += 5
}
Say "WSL $distribution is at $ip"

Forward 'ssh' '0.0.0.0' 2222 22 $ip

# The house cache's forward exists only on the host that serves it (docs/house-cache.md). Its listen address is the
# host's own on the house's network, kept as found; only the WSL side moves. The cache refuses to bind an address WSL
# doesn't hold, so its listen line moves with it and `loom house-cache install` restarts it on the new one, run as the
# user whose ~/.loom holds the cache's settings (Cloud's WSL has other users, and its default may be another).
$cache = netsh interface portproxy show v4tov4 | Select-String '^\s*(\S+)\s+7380\s+\S+\s+7380\s*$'
foreach ($line in $cache) {
	$listen = $line.Matches[0].Groups[1].Value
	Forward 'house cache' $listen 7380 7380 $ip
	$settings = wsl -d $distribution --cd / -u root -- sh -c 'ls /home/*/.loom/house-cache.conf' 2>&1
	if ("$settings" -notmatch '^/home/([a-z_][a-z0-9_-]*)/\.loom/house-cache\.conf$') {
		Say "house cache: no single user's ~/.loom/house-cache.conf ('$("$settings".Trim())'); its listen line is left as it was"
		continue
	}
	$user = $Matches[1]
	$install = "export XDG_RUNTIME_DIR=/run/user/`$(id -u); sed -i 's/^listen = .*/listen = ${ip}:7380/' ~/.loom/house-cache.conf && ~/.loom/bin/loom house-cache install"
	wsl -d $distribution --cd / -u $user -- bash -c $install 2>&1 | ForEach-Object { Say "house cache, as ${user}: $_" }
	Say "house cache: the install as $user exited $LASTEXITCODE"
}
