module github.com/b97tsk/chrome

go 1.15

require (
	github.com/fsnotify/fsnotify v1.4.9
	github.com/golang/protobuf v1.4.3
	github.com/gorilla/websocket v1.4.2
	github.com/shadowsocks/go-shadowsocks2 v0.1.3
	golang.org/x/net v0.0.0-20201031054903-ff519b6c9102
	golang.org/x/time v0.0.0-20201208040808-7e3f01d25324
	gopkg.in/yaml.v2 v2.3.0
	v2ray.com/core v4.19.1+incompatible
)

replace v2ray.com/core => github.com/v2fly/v2ray-core v4.32.1+incompatible
