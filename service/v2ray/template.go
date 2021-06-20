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
  "inbounds": [
    {
{{- if .ListenHost }}
      "listen": {{ .ListenHost | json }},
{{- end }}
      "port": {{ .ListenPort | json }},
{{- if eq $protocol "TROJAN" }}

{{- with .TROJAN }}
      "protocol": "trojan",
      "settings": {
        "clients": {{ .Clients | json }}
      },
{{- end }}{{/* with .TROJAN */}}

{{- else if eq $protocol "VMESS" }}

{{- with .VMESS }}
      "protocol": "vmess",
      "settings": {
        "clients": {{ .Clients | json }}
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
        "tlsSettings": {{ .TLS | json }},
        "sockopt": {
          "tcpFastOpen": true
        }
      }
    }
  ],
  "outbounds": [
    {
{{- if .ForwardServer.Address }}
      "protocol": "socks",
      "settings": {
        "servers": [
          {
{{- with .ForwardServer }}
            "address": {{ .Address | json }},
            "port": {{ .Port }}
{{- end }}{{/* with .ForwardServer */}}
          }
        ]
      }
{{- else }}
      "protocol": "freedom"
{{- end }}
    }
  ],
  "policy": {
    "levels": {
      "0": {{ .Policy | json }}
    }
  }
}
`
