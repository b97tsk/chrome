package vmess

import (
	"text/template"
)

var vmessTemplate *template.Template

func init() {
	vmessTemplate = template.New("vmess")
	template.Must(vmessTemplate.New("h2/tls").Parse(vmess_h2_tls_jsonString))
	template.Must(vmessTemplate.New("kcp").Parse(vmess_kcp_jsonString))
	template.Must(vmessTemplate.New("tcp").Parse(vmess_tcp_jsonString))
	template.Must(vmessTemplate.New("tcp/http").Parse(vmess_tcp_http_jsonString))
	template.Must(vmessTemplate.New("tcp/tls").Parse(vmess_tcp_tls_jsonString))
	template.Must(vmessTemplate.New("ws").Parse(vmess_ws_jsonString))
	template.Must(vmessTemplate.New("ws/tls").Parse(vmess_ws_tls_jsonString))
}

var vmess_h2_tls_jsonString = `
{
  "inbound": {
    "port": {{.LocalPort}},
    "listen": "{{.LocalAddr}}",
    "protocol": "socks",
    "settings": {}
  },
  "outbound": {
    "protocol": "vmess",
    "settings": {
      "vnext": [
        {
          "address": "{{.Address}}",
          "port": {{.Port}},
          "users": [
            {
              "id": "{{.ID}}",
              "alterId": {{.AlterID}},
              "security": "auto",
              "level": 0
            }
          ]
        }
      ]
    },
    "streamSettings": {
      "network": "h2",
      "security": "tls",
      "h2Settings": {
        "host": ["{{.Host}}"],
        "path": "{{.Path}}"
      }
    }
  },
  "policy": {
    "levels": {
      "0": {"uplinkOnly": 0}
    }
  }
}
`

var vmess_kcp_jsonString = `
{
  "inbound": {
    "port": {{.LocalPort}},
    "listen": "{{.LocalAddr}}",
    "protocol": "socks",
    "settings": {}
  },
  "outbound": {
    "protocol": "vmess",
    "settings": {
      "vnext": [
        {
          "address": "{{.Address}}",
          "port": {{.Port}},
          "users": [
            {
              "id": "{{.ID}}",
              "alterId": {{.AlterID}},
              "security": "auto",
              "level": 0
            }
          ]
        }
      ]
    },
    "streamSettings": {
      "network": "kcp",
      "kcpSettings": {
        "mtu": 1350,
        "tti": 50,
        "uplinkCapacity": 2,
        "downlinkCapacity": 100,
        "congestion": false,
        "readBufferSize": 2,
        "writeBufferSize": 2,
        "header": {"type": "{{.Type}}"}
      }
    }
  },
  "policy": {
    "levels": {
      "0": {"uplinkOnly": 0}
    }
  }
}
`

var vmess_tcp_jsonString = `
{
  "inbound": {
    "port": {{.LocalPort}},
    "listen": "{{.LocalAddr}}",
    "protocol": "socks",
    "settings": {}
  },
  "outbound": {
    "protocol": "vmess",
    "settings": {
      "vnext": [
        {
          "address": "{{.Address}}",
          "port": {{.Port}},
          "users": [
            {
              "id": "{{.ID}}",
              "alterId": {{.AlterID}},
              "security": "auto",
              "level": 0
            }
          ]
        }
      ]
    },
    "streamSettings": {
      "network": "tcp"
    }
  },
  "policy": {
    "levels": {
      "0": {"uplinkOnly": 0}
    }
  }
}
`

var vmess_tcp_http_jsonString = `
{
  "inbound": {
    "port": {{.LocalPort}},
    "listen": "{{.LocalAddr}}",
    "protocol": "socks",
    "settings": {}
  },
  "outbound": {
    "protocol": "vmess",
    "settings": {
      "vnext": [
        {
          "address": "{{.Address}}",
          "port": {{.Port}},
          "users": [
            {
              "id": "{{.ID}}",
              "alterId": {{.AlterID}},
              "security": "auto",
              "level": 0
            }
          ]
        }
      ]
    },
    "streamSettings": {
      "network": "tcp",
      "tcpSettings": {
        "header": {
          "type": "http",
          "request": {
            "version": "1.1",
            "method": "GET",
            "path": ["{{.Path}}"],
            "headers": {
              "Host": ["{{.Host}}"]
            }
          }
        }
      }
    }
  },
  "policy": {
    "levels": {
      "0": {"uplinkOnly": 0}
    }
  }
}
`

var vmess_tcp_tls_jsonString = `
{
  "inbound": {
    "port": {{.LocalPort}},
    "listen": "{{.LocalAddr}}",
    "protocol": "socks",
    "settings": {}
  },
  "outbound": {
    "protocol": "vmess",
    "settings": {
      "vnext": [
        {
          "address": "{{.Address}}",
          "port": {{.Port}},
          "users": [
            {
              "id": "{{.ID}}",
              "alterId": {{.AlterID}},
              "security": "auto",
              "level": 0
            }
          ]
        }
      ]
    },
    "streamSettings": {
      "network": "tcp",
      "security": "tls",
      "tlsSettings": {
        "serverName": "{{.Host}}",
        "allowInsecure": false
      }
    }
  },
  "policy": {
    "levels": {
      "0": {"uplinkOnly": 0}
    }
  }
}
`

var vmess_ws_jsonString = `
{
  "inbound": {
    "port": {{.LocalPort}},
    "listen": "{{.LocalAddr}}",
    "protocol": "socks",
    "settings": {}
  },
  "outbound": {
    "protocol": "vmess",
    "settings": {
      "vnext": [
        {
          "address": "{{.Address}}",
          "port": {{.Port}},
          "users": [
            {
              "id": "{{.ID}}",
              "alterId": {{.AlterID}},
              "security": "auto",
              "level": 0
            }
          ]
        }
      ]
    },
    "streamSettings": {
      "network": "ws",
      "wsSettings": {
        "path": "{{.Path}}",
        "headers": {}
      }
    }
  },
  "policy": {
    "levels": {
      "0": {"uplinkOnly": 0}
    }
  }
}
`

var vmess_ws_tls_jsonString = `
{
  "inbound": {
    "port": {{.LocalPort}},
    "listen": "{{.LocalAddr}}",
    "protocol": "socks",
    "settings": {}
  },
  "outbound": {
    "protocol": "vmess",
    "settings": {
      "vnext": [
        {
          "address": "{{.Address}}",
          "port": {{.Port}},
          "users": [
            {
              "id": "{{.ID}}",
              "alterId": {{.AlterID}},
              "security": "auto",
              "level": 0
            }
          ]
        }
      ]
    },
    "streamSettings": {
      "network": "ws",
      "security": "tls",
      "wsSettings": {
        "path": "{{.Path}}",
        "headers": {"Host": "{{.Host}}"}
      }
    }
  },
  "policy": {
    "levels": {
      "0": {"uplinkOnly": 0}
    }
  }
}
`
