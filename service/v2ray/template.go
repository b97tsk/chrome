package v2ray

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
	v2rayTemplate = template.New("v2ray").Funcs(funcs)
	template.Must(v2rayTemplate.Parse(v2rayTemplateBody))
}

var v2rayTemplate *template.Template

const v2rayTemplateBody = `
{{- $protocol := .Protocol -}}
{{- $transport := .Transport -}}
{
  "log": {
    "loglevel": "none"
  },
  "outbound": {
{{- if eq $protocol "freedom" }}

{{- with .FREEDOM }}
    "protocol": "freedom",
    "settings": {{ json . }},
{{- end }}{{/* with .FREEDOM */}}

{{- else if eq $protocol "vmess" }}

{{- with .VMESS }}
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
{{- end }}{{/* with .VMESS */}}

{{- end }}
    "streamSettings": {
{{- if eq $transport "http" }}

{{- with .HTTP }}
      "network": "http",
      "security": "tls",
      "httpSettings": {
        "host": {{ .Host | json }},
        "path": "{{ .Path }}"
      },
{{- end }}{{/* with .HTTP */}}

{{- else if eq $transport "kcp" }}

{{- with .KCP }}
      "network": "kcp",
      "kcpSettings": {
        "mtu": 1350,
        "tti": 50,
        "uplinkCapacity": 2,
        "downlinkCapacity": 100,
        "congestion": false,
        "readBufferSize": 2,
        "writeBufferSize": 2,
        "header": {
          "type": "{{ .Header }}"
        }
      },
{{- end }}{{/* with .KCP */}}

{{- else if eq $transport "tcp" "tcp/tls" }}

{{- with .TCP }}
      "network": "tcp",
{{- if eq $transport "tcp/tls" }}
      "security": "tls",
{{- end }}
{{- end }}{{/* with .TCP */}}

{{- else if eq $transport "ws" "ws/tls" }}

{{- with .WS }}
      "network": "ws",
{{- if eq $transport "ws/tls" }}
      "security": "tls",
{{- end }}
      "wsSettings": {
        "path": "{{ .Path }}",
        "headers": {{ .Header | json }}
      },
{{- end }}{{/* with .WS */}}

{{- end }}
{{- if ne .TLS.ServerName "" }}
      "tlsSettings": {{ .TLS | json }},
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
