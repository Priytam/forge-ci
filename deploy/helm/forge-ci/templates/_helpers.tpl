{{/*
Chart name (respecting nameOverride).
*/}}
{{- define "forge-ci.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Fully-qualified app name. Respects fullnameOverride, otherwise <release>-<name>.
*/}}
{{- define "forge-ci.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Chart label "name-version".
*/}}
{{- define "forge-ci.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Common labels applied to every object.
*/}}
{{- define "forge-ci.labels" -}}
helm.sh/chart: {{ include "forge-ci.chart" . }}
app.kubernetes.io/name: {{ include "forge-ci.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: forge-ci
{{- end -}}

{{/*
Per-component selector labels. Selectors are immutable, so these MUST stay
stable across upgrades — they intentionally exclude version/chart labels.
*/}}
{{- define "forge-ci.server.selectorLabels" -}}
app.kubernetes.io/name: {{ include "forge-ci.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: server
{{- end -}}

{{- define "forge-ci.web.selectorLabels" -}}
app.kubernetes.io/name: {{ include "forge-ci.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: web
{{- end -}}

{{- define "forge-ci.runner.selectorLabels" -}}
app.kubernetes.io/name: {{ include "forge-ci.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: runner
{{- end -}}

{{/*
Resource names for each component.
*/}}
{{- define "forge-ci.server.fullname" -}}
{{- printf "%s-server" (include "forge-ci.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "forge-ci.web.fullname" -}}
{{- printf "%s-web" (include "forge-ci.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "forge-ci.runner.fullname" -}}
{{- printf "%s-runner" (include "forge-ci.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Build a fully-qualified image reference from a component's repository + tag,
prefixing image.registry when set. Falls back to .Chart.AppVersion for the tag.
Usage: include "forge-ci.image" (dict "root" $ "repo" .Values.server.image.repository "tag" .Values.server.image.tag)
*/}}
{{- define "forge-ci.image" -}}
{{- $reg := .root.Values.image.registry -}}
{{- $repo := .repo -}}
{{- $tag := .tag | default .root.Chart.AppVersion -}}
{{- if $reg -}}
{{- printf "%s/%s:%s" $reg $repo $tag -}}
{{- else -}}
{{- printf "%s:%s" $repo $tag -}}
{{- end -}}
{{- end -}}

{{/*
Name of the Secret holding sensitive env. Returns existingSecret when provided,
otherwise the generated Secret's name.
*/}}
{{- define "forge-ci.secretName" -}}
{{- if .Values.secrets.existingSecret -}}
{{- .Values.secrets.existingSecret -}}
{{- else -}}
{{- printf "%s-secret" (include "forge-ci.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/*
Name of the non-secret env ConfigMap.
*/}}
{{- define "forge-ci.configMapName" -}}
{{- printf "%s-config" (include "forge-ci.fullname" .) -}}
{{- end -}}

{{/*
Assembled DATABASE_URL. When postgresql.enabled, build it from the bitnami
subchart's service + credentials; otherwise use the operator-supplied
database.url (managed DB). Only used when generating the Secret — with an
existingSecret the operator supplies DATABASE_URL themselves.
*/}}
{{- define "forge-ci.databaseUrl" -}}
{{- if .Values.postgresql.enabled -}}
{{- $host := printf "%s-postgresql" .Release.Name -}}
{{- printf "postgres://%s:%s@%s:5432/%s?sslmode=disable" .Values.postgresql.auth.username .Values.postgresql.auth.password $host .Values.postgresql.auth.database -}}
{{- else -}}
{{- required "database.url is required when postgresql.enabled=false and no existingSecret is set" .Values.database.url -}}
{{- end -}}
{{- end -}}

{{/*
ServiceAccount name for the server component.
*/}}
{{- define "forge-ci.server.serviceAccountName" -}}
{{- if .Values.server.serviceAccount.create -}}
{{- default (include "forge-ci.server.fullname" .) .Values.server.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.server.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
ServiceAccount name for the runner. In kubernetes mode this SA is bound to the
Role granting pod-management verbs.
*/}}
{{- define "forge-ci.runner.serviceAccountName" -}}
{{- if .Values.runner.serviceAccount.create -}}
{{- default (include "forge-ci.runner.fullname" .) .Values.runner.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.runner.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
The namespace job pods are created in (kubernetes executor). Defaults to the
release namespace.
*/}}
{{- define "forge-ci.runner.kubeNamespace" -}}
{{- default .Release.Namespace .Values.runner.kube.namespace -}}
{{- end -}}

{{/*
In-cluster server URL the runner uses to reach the control plane.
*/}}
{{- define "forge-ci.serverInternalURL" -}}
{{- printf "http://%s:%d" (include "forge-ci.server.fullname" .) (int .Values.server.service.port) -}}
{{- end -}}
