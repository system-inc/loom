# The boxes' Windows hosts

Every Linux box of the house (Workshop, Cloud, Server, Home and Chonchon) is WSL2 on a Windows host, in WSL's default network mode: WSL sits behind the host's own address translation on a private address (172.16.0.0/12) that changes every time WSL starts. Nothing on the house's network reaches WSL directly. Windows forwards a port to it (`netsh interface portproxy`), and only the ports forwarded answer.

| Forward | Host | What it carries |
|---|---|---|
| `0.0.0.0:2222` to WSL's `:22` | every box | ssh (`Port 2222` in the Mac's `~/.ssh/config`) |
| `10.10.102.20:7380` to WSL's `:7380` | Cloud | the house cache (docs/house-cache.md), with a firewall rule letting 10.10.0.0/16 in on TCP 7380 |

Since WSL's address changes on every start, the forwards are rewritten at every Windows start by `serving/windows/wsl-forward.ps1`, which the `WSL SSH forward` scheduled task runs: it starts WSL, reads eth0's address, points 2222 at it, and on a host with a 7380 forward points that at it too and moves the house cache's `listen` line with it (`loom house-cache install`, as the user whose `~/.loom` holds `house-cache.conf`, restarts the cache on the new address). Each forward is an `add` over the old entry (a `set` if a netsh refuses that), never a delete first, so the last boot's forward stands until the new one is in; every netsh call is checked and retried every 10 s until it takes, and with no address yet it keeps waiting rather than end, since ssh is the only way back into a box. It logs one line per action to `C:\wsl-forward.log`, with netsh's and the install's own words. A WSL restart without a Windows restart (`wsl --shutdown`) changes the address too, so run the task by hand after one: `schtasks /run /tn "WSL SSH forward"`.

## Installing

From the box's WSL, as the box's sudoer, with the host's Windows session an administrator (as every house box's is). Interop is on and C: isn't mounted, so mount it read-only for the calls and unmount after.

```bash
sudo mkdir -p /tmp/c && sudo mount -t drvfs C: /tmp/c -o ro
cp serving/windows/wsl-forward.ps1 /tmp/wsl-forward.ps1                 # from a checkout of loom on the box
/tmp/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe -NoProfile -Command \
  "Copy-Item '\\wsl.localhost\Ubuntu-24.04\tmp\wsl-forward.ps1' 'C:\wsl-forward.ps1' -Force"
/tmp/c/Windows/System32/schtasks.exe /create /f /tn "WSL SSH forward" /sc onstart /rl highest \
  /tr "powershell.exe -NoProfile -ExecutionPolicy Bypass -File C:\wsl-forward.ps1"
/tmp/c/Windows/System32/schtasks.exe /run /tn "WSL SSH forward"
sleep 20; tail -5 /tmp/c/wsl-forward.log                                  # "WSL Ubuntu-24.04 is at ...", "ssh: ..."
/tmp/c/Windows/System32/netsh.exe interface portproxy show all
sudo umount /tmp/c
```

The task runs as the host's own user, at start, with the highest privileges, since `netsh interface portproxy` needs an administrator. The older script it replaces, `C:\wsl-ssh-forward.ps1` (2222 only, a fixed 5 s wait), may stay on disk; the task no longer names it.

The house cache's forward and firewall rule are made once, by hand, on its host (Cloud):

```powershell
netsh interface portproxy add v4tov4 listenaddress=10.10.102.20 listenport=7380 connectaddress=<WSL's address> connectport=7380
New-NetFirewallRule -DisplayName "Loom house cache 7380" -Direction Inbound -Protocol TCP -LocalPort 7380 -RemoteAddress 10.10.0.0/16 -Action Allow
```

Undo: `netsh interface portproxy delete v4tov4 listenaddress=10.10.102.20 listenport=7380` and `Remove-NetFirewallRule -DisplayName "Loom house cache 7380"`. Once the forward is gone, `wsl-forward.ps1` leaves 7380 alone.

## Receive segment coalescing

Windows' receive segment coalescing on a host's physical card merges a fast sender's back-to-back segments, and the address translation into WSL drops the merged ones: a fast stream into WSL stalls after its first window while the host itself reads it at full speed (Server, Oct 10, #yz4c13w: a 12.7 MB blob from Cloud took 15 s for 78 KB, and 0.08 s once coalescing was off). Server's 10G card has it off: `Disable-NetAdapterRsc -Name 'Ethernet (10 GbE)'` (undo `Enable-NetAdapterRsc -Name 'Ethernet (10 GbE)'`), which survives a reboot. The other hosts still have it on and aren't hit today; a WSL that stalls receiving from the house gets the same setting on its host's card.
