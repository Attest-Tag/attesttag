{{- define "attest-tag.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "attest-tag.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "attest-tag.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "attest-tag.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "attest-tag.selectorLabels" . }}
app.kubernetes.io/version: {{ .Values.image.tag | default .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "attest-tag.selectorLabels" -}}
app.kubernetes.io/name: {{ include "attest-tag.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "attest-tag.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "attest-tag.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "attest-tag.workerServiceAccountName" -}}
{{- if .Values.worker.serviceAccount.create -}}
{{- default (printf "%s-worker" (include "attest-tag.fullname" .)) .Values.worker.serviceAccount.name -}}
{{- else -}}
{{- .Values.worker.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/* The worker image, tagged the same way the bot's is: worker.image.tag, else the appVersion. */}}
{{- define "attest-tag.workerImage" -}}
{{- printf "%s:%s" .Values.worker.image.repository (.Values.worker.image.tag | default .Chart.AppVersion) -}}
{{- end -}}

{{- define "attest-tag.secretName" -}}
{{- if .Values.existingSecret -}}
{{- .Values.existingSecret -}}
{{- else -}}
{{- include "attest-tag.fullname" . -}}
{{- end -}}
{{- end -}}

{{/*
The public origin. Set ADMIN_BASE_URL explicitly, or leave it empty and let the ingress host
supply it — those two disagreeing is the failure that sends password-reset links to the wrong
place, so there is one expression for it rather than two.
*/}}
{{- define "attest-tag.baseURL" -}}
{{- if .Values.config.ADMIN_BASE_URL -}}
{{- .Values.config.ADMIN_BASE_URL | trimSuffix "/" -}}
{{- else if .Values.ingress.host -}}
{{- printf "https://%s" .Values.ingress.host -}}
{{- end -}}
{{- end -}}

{{/*
Whether this release keeps anything on a local volume. That single question decides the shape:
with no local state it is a Deployment and may have several replicas; with any, it is a
StatefulSet pinned to one, because a ReadWriteOnce volume has exactly one writer.
*/}}
{{- define "attest-tag.usesSQLite" -}}
{{- if or .Values.database.url .Values.postgres.enabled }}false{{ else }}true{{ end -}}
{{- end -}}

{{- define "attest-tag.usesLocalDocs" -}}
{{- if or .Values.documents.s3.url .Values.minio.enabled }}false{{ else }}true{{ end -}}
{{- end -}}

{{/* The two things this chart can run for you, named after the release that runs them. */}}
{{- define "attest-tag.postgresName" -}}
{{- printf "%s-postgres" (include "attest-tag.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "attest-tag.minioName" -}}
{{- printf "%s-minio" (include "attest-tag.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
A password for something this chart runs itself.

Set it in values and that is what is used. Leave it empty and one is generated — and found again
on every upgrade by reading the Secret this release already wrote, because a password regenerated
on upgrade would lock the release out of its own database, which is the classic way a chart
destroys the thing it installed.

It is memoised into .Values on first use: randAlphaNum returns something different every time it
is called, and three templates ask for this one value.
*/}}
{{- define "attest-tag.bundledPassword" -}}
{{- $root := .root -}}
{{- if not (index .into .field) -}}
  {{- $found := "" -}}
  {{- $sec := lookup "v1" "Secret" $root.Release.Namespace (include "attest-tag.secretName" $root) -}}
  {{- if $sec -}}
    {{- with (index $sec.data .key) -}}{{- $found = b64dec . -}}{{- end -}}
  {{- end -}}
  {{- if and (not $found) $root.Values.existingSecret -}}
    {{- /* We cannot write into a Secret we do not own, and there is nothing in it to read. */ -}}
    {{- if $sec -}}
      {{- fail (printf "existingSecret %q has no %s, and this release runs its own %s. Add that key to your Secret, or bring storage of your own instead." $root.Values.existingSecret .key .what) -}}
    {{- end -}}
  {{- end -}}
  {{- if not $found -}}{{- $found = randAlphaNum 32 -}}{{- end -}}
  {{- $_ := set .into .field $found -}}
{{- end -}}
{{- index .into .field -}}
{{- end -}}

{{- define "attest-tag.postgresPassword" -}}
{{- include "attest-tag.bundledPassword" (dict "root" . "into" .Values.postgres "field" "password" "key" "POSTGRES_PASSWORD" "what" "Postgres") -}}
{{- end -}}

{{- define "attest-tag.minioPassword" -}}
{{- include "attest-tag.bundledPassword" (dict "root" . "into" .Values.minio "field" "rootPassword" "key" "MINIO_ROOT_PASSWORD" "what" "MinIO") -}}
{{- end -}}

{{/*
The bucket URL for a MinIO this chart runs: an in-cluster Service, plain http, and a region
because the signature needs one even where the store ignores it.
*/}}
{{- define "attest-tag.minioURL" -}}
{{- printf "s3://%s/docs?endpoint=http://%s:9000&region=us-east-1" .Values.minio.bucket (include "attest-tag.minioName" .) -}}
{{- end -}}

{{- define "attest-tag.stateful" -}}
{{- if or (eq (include "attest-tag.usesSQLite" .) "true") (eq (include "attest-tag.usesLocalDocs" .) "true") }}true{{ else }}false{{ end -}}
{{- end -}}
