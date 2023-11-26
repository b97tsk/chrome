package v2ray

import (
	"encoding/json"
	"text/template"
)

func init() {
	funcs := template.FuncMap{
		"json": func(v any) (string, error) {
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
{{- $security := .Security -}}
{
	"log": {
		"access": { "type": "None" },
		"error": { "type": "None" }
	},
	"inbounds": [
		{
{{- if .ListenHost }}
			"listen": {{ .ListenHost | json }},
{{- end }}
			"port": {{ .ListenPort | json }},
{{- if eq $protocol "SHADOWSOCKS" }}
			"protocol": "shadowsocks",
			"settings": {{ .SHADOWSOCKS | json }},
{{- else if eq $protocol "TROJAN" }}
			"protocol": "trojan",
			"settings": {{ .TROJAN | json }},
{{- else if eq $protocol "VLESS" }}
			"protocol": "vless",
			"settings": {{ .VLESS | json }},
{{- else if eq $protocol "VMESS" }}
			"protocol": "vmess",
			"settings": {{ .VMESS | json }},
{{- end }}
			"streamSettings": {
{{- if eq $transport "GRPC" }}
				"transport": "grpc",
				"transportSettings": {{ .GRPC | json }},
{{- else if eq $transport "TCP" }}
				"transport": "tcp",
				"transportSettings": {{ .TCP | json }},
{{- else if eq $transport "WS" }}
				"transport": "ws",
				"transportSettings": {{ .WS | json }},
{{- end }}
{{- if eq $security "TLS" }}
				"security": "tls",
				"securitySettings": {{ .TLS | json }},
{{- end }}
				"socketSettings": {}
			}
		}
	],
	"outbounds": [
		{
{{- if .ForwardServer.Address }}
			"protocol": "socks",
			"settings": {{ .ForwardServer | json }}
{{- else }}
			"protocol": "freedom"
{{- end }}
		}
	]
}
`
