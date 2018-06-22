# chrome
Services implemented by Go

# Install
```console
go get -u github.com/b97tsk/chrome
```

# Usage
Create a config file (see below), then run:
```console
chrome -conf path/to/a/config/file
```
By default, running:
```console
chrome
```
Exactly like running:
```console
chrome -conf chrome.yaml
```

# Config File Sample
```yaml
logging: ${ConfigDir}/chrome.log # write log messages to this file
aliases:
  - &SS         ss://aes-256-cfb:password@example.com:12345
  - &Tor        socks5://127.0.0.1:9150
  - &SSOverTor  [*SS, *Tor]
httpfs: # HTTP file servers, very simple
  127.1.2.7:80: {dir: .}
  127.1.2.7:8080: {dir: $HOME}
socks: # SOCKS5 servers
  127.1.2.7:1080: {over: *SS} # yet another shadowsocks client
  127.1.2.7:1081: {over: *Tor}
  127.1.2.7:1082: {over: *SSOverTor}
tcptun: # TCP tunnelings
  127.1.2.7:443: {for: www.google.com:443, over: *SS}
  127.1.2.7:5353: {for: 8.8.8.8:53, over: *Tor}
shadowsocks: # Shadowsocks servers
  127.1.2.7:10800: {method: aes-256-cfb, password: 123456, over: *SS}
  127.1.2.7:10801: {method: aes-256-cfb, password: 123456, over: *Tor}
```
