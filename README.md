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
  - &SS         ss://method:password@example.com:12345
  - &Tor        socks5://127.0.0.1:9150
  - &SSOverTor  [*SS, *Tor]

# HTTP file servers, very simple
httpfs|127.1.2.7|80: {dir: .}
httpfs|127.1.2.7|8080: {dir: $HOME}

# SOCKS5 servers
socks|127.1.2.7|1080: {over: *SS} # yet another shadowsocks client
socks|127.1.2.7|1081: {over: *Tor}
socks|127.1.2.7|1082: {over: *SSOverTor}

# TCP tunnelings
tcptun|127.1.2.7|443: {for: www.google.com:443, over: *SS}
tcptun|127.1.2.7|5353: {for: 8.8.8.8:53, over: *Tor}

# Shadowsocks servers
shadowsocks|127.1.2.7|10800: {method: aes-256-cfb, password: 123456, over: *SS}
shadowsocks|127.1.2.7|10801: {method: aes-256-cfb, password: 123456, over: *Tor}
```
