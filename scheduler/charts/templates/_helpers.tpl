{{- define "grove-kube-scheduler.config.data" -}}
config.yaml: |
  ---
  apiVersion: kubescheduler.config.k8s.io/v1
  kind: KubeSchedulerConfiguration
  leaderElection:
    # (Optional) Change true to false if you are not running a HA control-plane.
    leaderElect: false
  clientConnection:
    kubeconfig: /etc/kubernetes/scheduler.conf
{{- end -}}
{{- define "common.chart.labels" -}}
chart: "{{ .Chart.Name }}-{{ .Chart.Version }}"
release: "{{ .Release.Name }}"
{{- end -}}

{{- define "grove-kube-scheduler.config.name" -}}
grove-operator-cm-{{ include "grove-kube-scheduler.config.data" . | sha256sum | trunc 8 }}
{{- end -}}

{{- define "grove-kube-scheduler.config.lables" -}}
{{- include "common.chart.labels" . }}
{{- range $key, $val := .Values.configMap.labels }}
{{ $key }}: {{ $val }}
{{- end }}
{{- end -}}

{{- define "grove-kube-scheduler.deployment.matchLabels" -}}
{{- range $key, $val := .Values.deployment.labels }}
{{ $key }}: {{ $val }}
{{- end }}
{{- end -}}

{{- define "grove-kube-scheduler.deployment.labels" -}}
{{- include "common.chart.labels" . }}
{{- include "grove-kube-scheduler.deployment.matchLabels" . }}
{{- end -}}

{{- define "grove-kube-scheduler.serviceaccount.labels" -}}
{{- include "common.chart.labels" . }}
{{- range $key, $val := .Values.serviceAccount.labels }}
{{ $key }}: {{ $val }}
{{- end }}
{{- end -}}

{{- define "grove-kube-scheduler.clusterrole.labels" -}}
{{- include "common.chart.labels" . }}
{{- range $key, $val := .Values.clusterRole.labels }}
{{ $key }}: {{ $val }}
{{- end }}
{{- end -}}

{{- define "grove-kube-scheduler.clusterrolebinding.labels" -}}
{{- include "common.chart.labels" . }}
{{- range $key, $val := .Values.clusterRoleBinding.labels }}
{{ $key }}: {{ $val }}
{{- end }}
{{- end -}}

{{- define "grove-kube-scheduler.imagevector-overwrite-charts.name" -}}
{{- end -}}

{{- define "image" -}}
{{- if hasPrefix "sha256:" (required "$.tag is required" $.tag) -}}
{{ required "$.repository is required" $.repository }}@{{ required "$.tag" $.tag }}
{{- else -}}
{{ required "$.repository is required" $.repository }}:{{ required "$.tag" $.tag }}
{{- end -}}
{{- end -}}
