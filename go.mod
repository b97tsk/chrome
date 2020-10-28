module github.com/b97tsk/chrome

go 1.15

require (
	github.com/fsnotify/fsnotify v1.4.9
	github.com/golang/protobuf v1.4.3
	github.com/shadowsocks/go-shadowsocks2 v0.1.0
	golang.org/x/net v0.0.0-20201022231255-08b38378de70
	gopkg.in/yaml.v2 v2.3.0
	v2ray.com/core v4.19.1+incompatible
)

replace v2ray.com/core => github.com/v2fly/v2ray-core v4.31.3+incompatible
