package v2socks

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
	v2socksTemplate = template.New("v2socks").Funcs(funcs)
	template.Must(v2socksTemplate.Parse(v2socksTemplateBody))
}

var v2socksTemplate *template.Template

const v2socksTemplateBody = `
{{- $protocol := .Protocol -}}
{{- $transport := .Transport -}}
{
  "log": {
    "loglevel": "none"
  },
  "outbounds": [
    {
{{- if eq $protocol "TROJAN" }}

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
                "alterId": {{ .AlterID }}
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
      },
{{- if .ForwardServer.Address }}
      "proxySettings": {
		"tag": "fwd",
		"transportLayer": true
      },
{{- end }}
      "mux": {{ .Mux | json }}
{{- if .ForwardServer.Address }}
    },
    {
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
      },
	  "tag": "fwd"
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
