{{/*
Common labels
*/}}
{{- define "varroa.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "varroa.selectorLabels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Full name for a component
*/}}
{{- define "varroa.fullname" -}}
{{- printf "%s-%s" .Release.Name .name }}
{{- end }}

{{/*
Default resource requests/limits
*/}}
{{- define "varroa.defaultResources" -}}
requests:
  cpu: 100m
  memory: 128Mi
limits:
  cpu: 500m
  memory: 512Mi
{{- end }}

{{/*
NATS bus — memoized credential seed. Called once per render; caches all
credentials (per-service passwords, CA, TLS) on $ so every consumer gets
the same values. On upgrade reuses the existing Secret; on first install
generates everything fresh.
*/}}
{{- define "varroa.nats.creds" -}}
{{- if not (hasKey $ "natsCreds") -}}
  {{- $secretName := printf "%s-nats-creds" $.Release.Name -}}
  {{- $existing := lookup "v1" "Secret" $.Release.Namespace $secretName -}}
  {{- if $existing -}}
    {{- $_ := set $ "natsCreds" (dict
          "operator" (index $existing.data "operator-password" | b64dec)
          "gateway"  (index $existing.data "gateway-password"  | b64dec)
          "bff"      (index $existing.data "bff-password"       | b64dec)
          "caCert"   (index $existing.data "ca.crt"   | b64dec)
          "caKey"    (index $existing.data "ca.key"   | b64dec)
          "tlsCert"  (index $existing.data "tls.crt"  | b64dec)
          "tlsKey"   (index $existing.data "tls.key"  | b64dec)) -}}
  {{- else -}}
    {{- $ca := genCA (printf "%s-nats-ca" $.Release.Name) 365 -}}
    {{- $altNames := list (printf "%s-nats" $.Release.Name) (printf "%s-nats.%s.svc" $.Release.Name $.Release.Namespace) (printf "%s-nats.%s.svc.cluster.local" $.Release.Name $.Release.Namespace) (printf "*.%s-nats.%s.svc.cluster.local" $.Release.Name $.Release.Namespace) -}}
    {{- $external := .Values.nats.external | default dict -}}
    {{- if $external.host -}}
      {{- $altNames = append $altNames $external.host -}}
    {{- end -}}
    {{- range $external.extraSANs -}}
      {{- $altNames = append $altNames . -}}
    {{- end -}}
    {{- $ipSANs := $external.ipSANs | default list -}}
    {{- $cert := genSignedCert (printf "%s-nats" $.Release.Name) $ipSANs $altNames 365 $ca -}}
    {{- $_ := set $ "natsCreds" (dict
          "operator" (randAlphaNum 32)
          "gateway"  (randAlphaNum 32)
          "bff"      (randAlphaNum 32)
          "caCert"   $ca.Cert
          "caKey"    $ca.Key
          "tlsCert"  $cert.Cert
          "tlsKey"   $cert.Key) -}}
  {{- end -}}
{{- end -}}
{{- end -}}

{{- define "varroa.nats.operatorPassword" -}}{{- include "varroa.nats.creds" $ -}}{{- $.natsCreds.operator -}}{{- end -}}
{{- define "varroa.nats.gatewayPassword" -}}{{- include "varroa.nats.creds" $ -}}{{- $.natsCreds.gateway -}}{{- end -}}
{{- define "varroa.nats.bffPassword"     -}}{{- include "varroa.nats.creds" $ -}}{{- $.natsCreds.bff     -}}{{- end -}}
{{- define "varroa.nats.caCert"          -}}{{- include "varroa.nats.creds" $ -}}{{- $.natsCreds.caCert  -}}{{- end -}}
{{- define "varroa.nats.caKey"           -}}{{- include "varroa.nats.creds" $ -}}{{- $.natsCreds.caKey   -}}{{- end -}}
{{- define "varroa.nats.tlsCert"         -}}{{- include "varroa.nats.creds" $ -}}{{- $.natsCreds.tlsCert -}}{{- end -}}
{{- define "varroa.nats.tlsKey"          -}}{{- include "varroa.nats.creds" $ -}}{{- $.natsCreds.tlsKey  -}}{{- end -}}

{{/*
Hive-mode helper: returns "true" when .Values.mode is "hive".
*/}}
{{- define "varroa.isHive" -}}{{- if eq (.Values.mode | default "full") "hive" -}}true{{- end -}}{{- end -}}

{{/*
Bus URL resolution: per-tier value → global bus.url → tier-specific
busURL → in-cluster default nats://<release>-nats:4222.
Usage: {{ include "varroa.busURL" (dict "root" $ "tier" "operator") }}
*/}}
{{- define "varroa.busURL" -}}
{{- $tier := .tier -}}{{- $root := .root -}}
{{- $root.Values.bus.url | default (index $root.Values $tier "busURL") | default (printf "nats://%s-nats:4222" $root.Release.Name) -}}
{{- end -}}

{{/*
Actual NATS server count the nats-1.2.0 subchart will render, per its own
stateful-set.yaml: replicas = .Values.config.cluster.replicas when
.Values.config.cluster.enabled, else always 1 (a disabled cluster stanza
hardcodes a single standalone server no matter what replicas says).
Usage: {{ include "varroa.natsServerCount" $ }}
*/}}
{{- define "varroa.natsServerCount" -}}
{{- if .Values.nats.config.cluster.enabled -}}
{{- int (.Values.nats.config.cluster.replicas | default 1) -}}
{{- else -}}
1
{{- end -}}
{{- end -}}

{{/*
JetStream replica count for streams and KV buckets (VARROA_JETSTREAM_REPLICAS).
Precedence: explicit .Values.jetStreamReplicas → derived from the REAL
clustering keys the nats-1.2.0 subchart reads, nats.config.cluster.enabled /
nats.config.cluster.replicas (see varroa.natsServerCount) — NOT the old decoy
top-level nats.replicas (issue #433), which the subchart never read.
The value is clamped to the range 1..3: JetStream requires >= 1 replica, and the
cap at 3 is intentional — JetStream R>3 is rarely useful, so a larger NATS cluster
still tops out at 3-way replication. Even hive-mode installs (nats.enabled=false)
render this from nats.config.cluster.replicas so they replicate against the
core's NATS cluster.
Usage: {{ include "varroa.jetStreamReplicas" $ }}
*/}}
{{- define "varroa.jetStreamReplicas" -}}
{{- $n := 1 -}}
{{- if kindIs "invalid" .Values.jetStreamReplicas -}}
{{-   $n = int (include "varroa.natsServerCount" $) -}}
{{- else -}}
{{-   $n = int .Values.jetStreamReplicas -}}
{{- end -}}
{{- if lt $n 1 -}}{{- $n = 1 -}}{{- end -}}
{{- if gt $n 3 -}}{{- $n = 3 -}}{{- end -}}
{{- $n -}}
{{- end -}}
