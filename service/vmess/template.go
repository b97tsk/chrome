package vmess

import (
	"text/template"
)

var vmessTemplate *template.Template

func init() {
	vmessTemplate = template.New("vmess")
	template.Must(vmessTemplate.New("h2/tls").Parse(h2TLSJSONString))
	template.Must(vmessTemplate.New("kcp").Parse(kcpJSONString))
	template.Must(vmessTemplate.New("tcp").Parse(tcpJSONString))
	template.Must(vmessTemplate.New("tcp/http").Parse(tcpHTTPJSONString))
	template.Must(vmessTemplate.New("tcp/tls").Parse(tcpTLSJSONString))
	template.Must(vmessTemplate.New("ws").Parse(wsJSONString))
	template.Must(vmessTemplate.New("ws/tls").Parse(wsTLSJSONString))
}

const h2TLSJSONString = `
{
  "log": {
    "loglevel": "none"
  },
  "inbound": {
    "listen": "{{.ListenHost}}",
    "port": {{.ListenPort}},
    "protocol": "socks"
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
    },
    "mux": {
      "enabled": {{.MuxEnabled}},
      "concurrency": {{.MuxConcurrency}}
    }
  },
  "policy": {
    "levels": {
      "0": {
        "uplinkOnly": 2,
        "downlinkOnly": 2
      }
    }
  }
}
`

const kcpJSONString = `
{
  "log": {
    "loglevel": "none"
  },
  "inbound": {
    "listen": "{{.ListenHost}}",
    "port": {{.ListenPort}},
    "protocol": "socks"
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
    },
    "mux": {
      "enabled": {{.MuxEnabled}},
      "concurrency": {{.MuxConcurrency}}
    }
  },
  "policy": {
    "levels": {
      "0": {
        "uplinkOnly": 2,
        "downlinkOnly": 2
      }
    }
  }
}
`

const tcpJSONString = `
{
  "log": {
    "loglevel": "none"
  },
  "inbound": {
    "listen": "{{.ListenHost}}",
    "port": {{.ListenPort}},
    "protocol": "socks"
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
    },
    "mux": {
      "enabled": {{.MuxEnabled}},
      "concurrency": {{.MuxConcurrency}}
    }
  },
  "policy": {
    "levels": {
      "0": {
        "uplinkOnly": 2,
        "downlinkOnly": 2
      }
    }
  }
}
`

const tcpHTTPJSONString = `
{
  "log": {
    "loglevel": "none"
  },
  "inbound": {
    "listen": "{{.ListenHost}}",
    "port": {{.ListenPort}},
    "protocol": "socks"
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
    },
    "mux": {
      "enabled": {{.MuxEnabled}},
      "concurrency": {{.MuxConcurrency}}
    }
  },
  "policy": {
    "levels": {
      "0": {
        "uplinkOnly": 2,
        "downlinkOnly": 2
      }
    }
  }
}
`

const tcpTLSJSONString = `
{
  "log": {
    "loglevel": "none"
  },
  "inbound": {
    "listen": "{{.ListenHost}}",
    "port": {{.ListenPort}},
    "protocol": "socks"
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
    },
    "mux": {
      "enabled": {{.MuxEnabled}},
      "concurrency": {{.MuxConcurrency}}
    }
  },
  "policy": {
    "levels": {
      "0": {
        "uplinkOnly": 2,
        "downlinkOnly": 2
      }
    }
  }
}
`

const wsJSONString = `
{
  "log": {
    "loglevel": "none"
  },
  "inbound": {
    "listen": "{{.ListenHost}}",
    "port": {{.ListenPort}},
    "protocol": "socks"
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
        "path": "{{.Path}}"
      }
    },
    "mux": {
      "enabled": {{.MuxEnabled}},
      "concurrency": {{.MuxConcurrency}}
    }
  },
  "policy": {
    "levels": {
      "0": {
        "uplinkOnly": 2,
        "downlinkOnly": 2
      }
    }
  }
}
`

const wsTLSJSONString = `
{
  "log": {
    "loglevel": "none"
  },
  "inbound": {
    "listen": "{{.ListenHost}}",
    "port": {{.ListenPort}},
    "protocol": "socks"
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
        "headers": {
          "Host": "{{.Host}}"
        }
      }
    },
    "mux": {
      "enabled": {{.MuxEnabled}},
      "concurrency": {{.MuxConcurrency}}
    }
  },
  "policy": {
    "levels": {
      "0": {
        "uplinkOnly": 2,
        "downlinkOnly": 2
      }
    }
  }
}
`
