package v2socks

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
	v2socksTemplate = template.New("v2socks").Funcs(funcs)
	template.Must(v2socksTemplate.Parse(v2socksTemplateBody))
}

var v2socksTemplate *template.Template

const v2socksTemplateBody = `
{{- $protocol := .Protocol -}}
{{- $transport := .Transport -}}
{{- $security := .Security -}}
{
	"log": {
		"access": { "type": "None" },
		"error": { "type": "None" }
	},
	"outbounds": [
		{
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
			"settings": {{ .ForwardServer | json }},
			"tag": "fwd"
{{- end }}
		}
	]
}
`
