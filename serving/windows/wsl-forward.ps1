# wsl-forward.ps1: the box's Windows host runs this at every start (the "WSL SSH forward" scheduled task,
# docs/windows-host.md). It starts the box's WSL, reads the address Windows gave it (a new one after every WSL start),
# and points this host's forwards at it: ssh's 2222 always, and on a host that serves the house cache, its 7380
# forward and the cache's own listen address inside WSL. One line per action goes to C:\wsl-forward.log.
$distribution = 'Ubuntu-24.04'
$log = 'C:\wsl-forward.log'
function Say($message) { Add-Content -Path $log -Value ('{0:yyyy-MM-ddTHH:mm:ssK} {1}' -f (Get-Date), $message) }

Start-Process wsl -ArgumentList "-d $distribution -- sleep infinity" -WindowStyle Hidden
$ip = $null
foreach ($attempt in 1..60) {
	$addresses = wsl -d $distribution --cd / hostname -I
	if ($addresses) { $ip = "$addresses".Trim().Split(' ')[0] }
	if ($ip) { break }
	Start-Sleep 1
}
if (-not $ip) {
	Say "WSL $distribution has no address after 60 s; the forwards are left as they were"
	exit 1
}
Say "WSL $distribution is at $ip"

netsh interface portproxy delete v4tov4 listenport=2222 listenaddress=0.0.0.0 2>$null | Out-Null
netsh interface portproxy add v4tov4 listenport=2222 listenaddress=0.0.0.0 connectport=22 connectaddress=$ip | Out-Null
Say "ssh: 0.0.0.0:2222 -> ${ip}:22"

# The house cache's forward exists only on the host that serves it (docs/house-cache.md). Its listen address is the
# host's own on the house's network, kept as found; only the WSL side moves. The cache refuses to bind an address WSL
# doesn't hold, so its listen line moves with it and `loom house-cache install` restarts it on the new one.
$cache = netsh interface portproxy show v4tov4 | Select-String '^\s*(\S+)\s+7380\s+\S+\s+7380\s*$'
foreach ($line in $cache) {
	$listen = $line.Matches[0].Groups[1].Value
	netsh interface portproxy delete v4tov4 listenport=7380 listenaddress=$listen | Out-Null
	netsh interface portproxy add v4tov4 listenport=7380 listenaddress=$listen connectport=7380 connectaddress=$ip | Out-Null
	Say "house cache: ${listen}:7380 -> ${ip}:7380"
	$install = "export XDG_RUNTIME_DIR=/run/user/`$(id -u); sed -i 's/^listen = .*/listen = ${ip}:7380/' ~/.loom/house-cache.conf && ~/.loom/bin/loom house-cache install"
	wsl -d $distribution --cd / -u ahra -- bash -c $install 2>&1 | ForEach-Object { Say "house cache: $_" }
}
