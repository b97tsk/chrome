# chrome

Services implemented by Go.

# Install

```console
# go get -u github.com/b97tsk/chrome
```

# Usage

Create a config file (see below), then run:

```console
chrome path/to/your/config/file
```

If config file is not specified, `chrome.yaml` is assumed.

If config file is `-`, chrome will try to load config from standard input.

# Sample Config File

```yaml
logging: ${ConfigDir}/chrome.log # Write log messages to this file.

alias#1: # Field names start with `alias` are ignored.
  - &SS ss://method:password@example.com:12345
  - &Tor socks5://127.0.0.1:9150

# Create three TCP tunnelings over different forward proxy.
tcptun|127.1.2.7|53: { for: 1.1.1.1:53 } # Direct access.
tcptun|127.1.2.7|54: { for: 8.8.8.8:53, over: *SS }
tcptun|127.1.2.7|443: { for: www.google.com:443, over: *Tor }

# Create three SOCKS5 servers over different forward proxy.
socks|127.1.2.7|1080: {} # Direct access.
socks|127.1.2.7|1081: { over: *SS }
socks|127.1.2.7|1082: { over: *Tor }

# The followings are the same as above three.
# As you can see, the listening ports are auto-increasing.
socks|127.1.2.7|1080+:
  - {}
  - over: *SS
  - over: *Tor

alias#2:
  - &Direct []
  - &SSOverTor [*SS, *Tor]

# Create three Shadowsocks servers, listening on all available IP addresses
# of the local system.
shadowsocks||10800+:
  - { method: aes-256-gcm, password: 123456, over: *SSOverTor }
  - { method: aes-256-gcm, password: 123456, over: *Direct }
  - { method: aes-256-gcm, password: !!binary MTIzNDU2 } # Same as above.
```
