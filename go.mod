module github.com/b97tsk/chrome

go 1.14

require (
	github.com/fsnotify/fsnotify v1.4.9
	github.com/gogo/protobuf v1.3.1
	github.com/kr/pretty v0.1.0 // indirect
	github.com/shadowsocks/go-shadowsocks2 v0.1.0
	golang.org/x/net v0.0.0-20200602114024-627f9648deb9
	gopkg.in/check.v1 v1.0.0-20180628173108-788fd7840127 // indirect
	gopkg.in/yaml.v2 v2.3.0
	v2ray.com/core v4.19.1+incompatible
)

replace v2ray.com/core => github.com/v2ray/v2ray-core v4.25.1+incompatible
