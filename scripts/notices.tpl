{{ range . }}@@@@ {{ .Name }}
================================================================
{{ .Name }} ({{ .LicenseName }})
{{ .LicenseURL }}

{{ .LicenseText }}
{{ end }}
