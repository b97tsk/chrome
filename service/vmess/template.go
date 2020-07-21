package vmess

import (
	"encoding/json"
	"text/template"
)

func init() {
	funcs := template.FuncMap{
		"json": func(v interface{}) (string, error) {
			bytes, err := json.Marshal(v)
			return string(bytes), err
		},
	}
	vmessTemplate = template.New("vmess").Funcs(funcs)
	template.Must(vmessTemplate.Parse(vmessTemplateBody))
}

var vmessTemplate *template.Template

const vmessTemplateBody = `
{
  "log": {
    "loglevel": "none"
  },
  "outbound": {
    "protocol": "vmess",
    "settings": {
      "vnext": [
        {
          "address": "{{ .Address }}",
          "port": {{ .Port }},
          "users": [
            {
              "id": "{{ .ID }}",
              "alterId": {{ .AlterID }},
              "security": "auto",
              "level": 0
            }
          ]
        }
      ]
    },
    "streamSettings": {
{{- if eq .Type "http" }}

{{- /******** HTTP BEGIN ********/}}
      "network": "http",
      "security": "tls",
      "httpSettings": {
        "host": {{ .HTTP.Host | json }},
        "path": "{{ .HTTP.Path }}"
      },
{{- /******** HTTP END ********/}}

{{- else if eq .Type "kcp" }}

{{- /******** KCP BEGIN ********/}}
      "network": "kcp",
      "kcpSettings": {
        "mtu": 1350,
        "tti": 50,
        "uplinkCapacity": 2,
        "downlinkCapacity": 100,
        "congestion": false,
        "readBufferSize": 2,
        "writeBufferSize": 2,
        "header": {"type": "{{ .KCP.Header }}"}
      },
{{- /******** KCP END ********/}}

{{- else if eq .Type "tcp" "tcp/tls" }}

{{- /******** TCP BEGIN ********/}}
      "network": "tcp",
{{- if eq .Type "tcp/tls" }}
      "security": "tls",
{{- end }}
{{- if ne .TCP.HTTP.Path "" }}
      "tcpSettings": {
        "header": {
          "type": "http",
          "request": {
            "version": "1.1",
            "method": "GET",
            "path": ["{{ .TCP.HTTP.Path }}"],
            "headers": {{ .TCP.HTTP.Header | json }}
          }
        }
      },
{{- end }}
{{- if eq .Type "tcp/tls" }}
      "tlsSettings": {
        "serverName": "{{ .TCP.TLS.ServerName }}",
        "allowInsecure": false
      },
{{- end }}
{{- /******** TCP END ********/}}

{{- else if eq .Type "ws" "ws/tls" }}

{{- /******** WS BEGIN ********/}}
      "network": "ws",
{{- if eq .Type "ws/tls" }}
      "security": "tls",
{{- end }}
      "wsSettings": {
        "path": "{{ .WS.Path }}",
        "headers": {{ .WS.Header | json }}
      },
{{- /******** WS END ********/}}

{{- end }}
      "sockopt": {
        "tcpFastOpen": true
      }
    },
    "mux": {{ .Mux | json }}
  },
  "policy": {
    "levels": {
      "0": {{ .Policy | json }}
    }
  }
}
`
