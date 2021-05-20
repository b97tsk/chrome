# chrome

Services implemented in Go.

# Install

```console
# go install github.com/b97tsk/chrome/cmd/chrome@latest
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
log:
  file: chrome.log # Write log messages to this file.
  level: INFO # Log level, alternatives are ERROR, WARN, DEBUG, TRACE and NONE.

dial:
  timeout: 10s # Default connect timeout.

alias#1: # Field names start with word `alias` are ignored.
  - &SS ss://method:password@example.com:12345
  - &Tor socks5://127.0.0.1:9150

# Create three TCP tunnels over different forward proxies.

tcptun#1: # Note that field names must be unique.
  on: 127.1.2.7:53 # Listen address.
  for: 1.1.1.1:53 # Forward target.
  over: Direct # Direct access, which is the default.

tcptun#2: # The first word is the service name, which must be valid.
  on: 127.1.2.7:5353
  for: 8.8.8.8:53
  over: *SS

tcptun#3:
  on: 127.1.2.7:443
  for: www.google.com:443
  over: *Tor

# Create three SOCKS5 servers over different forward proxies.

socks#1:
  on: 127.1.2.7:1080 # Listen address.
  over: Direct # Direct access, which is the default.

socks#2:
  on: 127.1.2.7:1081
  over: *SS

socks#3:
  on: 127.1.2.7:1082
  over: *Tor

# Create three Shadowsocks servers over different forward proxies.

shadowsocks#1:
  on: :10800 # Listening on all available IP addresses of the local system.
  method: aes-256-gcm
  password: 123456
  over: [] # Direct access, which is the default.

shadowsocks#2:
  on: :10801
  method: aes-256-gcm
  password: 123456
  over: [*SS, *Tor] # Chain proxies, one over another.

shadowsocks#3:
  on: :10802
  method: aes-256-gcm
  password: !!binary MTIzNDU2 # Encoded in Base64.
  over: [Direct, *SS, Direct] # `Direct`s are simply ignored.
```
