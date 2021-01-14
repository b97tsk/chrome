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
  "outbounds": [
    {
{{- if eq $protocol "FREEDOM" }}

{{- with .FREEDOM }}
      "protocol": "freedom",
      "settings": {{ json . }},
{{- end }}{{/* with .FREEDOM */}}

{{- else if eq $protocol "TROJAN" }}

{{- with .TROJAN }}
      "protocol": "trojan",
      "settings": {
        "servers": [
          {
            "address": {{ .Address | json }},
            "port": {{ .Port }},
            "password": {{ .Password | json }}
          }
        ]
      },
{{- end }}{{/* with .TROJAN */}}

{{- else if eq $protocol "VMESS" }}

{{- with .VMESS }}
      "protocol": "vmess",
      "settings": {
        "vnext": [
          {
            "address": {{ .Address | json }},
            "port": {{ .Port }},
            "users": [
              {
                "id": {{ .ID | json }},
                "alterId": {{ .AlterID }},
                "security": "auto"
              }
            ]
          }
        ]
      },
{{- end }}{{/* with .VMESS */}}

{{- end }}
      "streamSettings": {
{{- if eq $transport "HTTP" }}

{{- with .HTTP }}
        "network": "http",
        "security": "tls",
        "httpSettings": {
          "host": {{ .Host | json }},
          "path": {{ .Path | json }}
        },
{{- end }}{{/* with .HTTP */}}

{{- else if eq $transport "KCP" }}

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
            "type": {{ .Header | json }}
          }
        },
{{- end }}{{/* with .KCP */}}

{{- else if eq $transport "TCP" }}

{{- $tlsEnabled := .TLS.Enabled }}
{{- with .TCP }}
        "network": "tcp",
{{- if $tlsEnabled }}
        "security": "tls",
{{- end }}
{{- end }}{{/* with .TCP */}}

{{- else if eq $transport "WS" }}

{{- $tlsEnabled := .TLS.Enabled }}
{{- with .WS }}
        "network": "ws",
{{- if $tlsEnabled }}
        "security": "tls",
{{- end }}
        "wsSettings": {
          "path": {{ .Path | json }},
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
    }
  ],
  "policy": {
    "levels": {
      "0": {{ .Policy | json }}
    }
  }
}
`
