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
Sprig cannot read a certificate, so three annotations on the Secret record
what the stored certificates hold: varroa.dev/nats-tls-sans (the SAN list),
varroa.dev/nats-tls-not-after, and varroa.dev/nats-ca-not-after (RFC 3339,
UTC). An absent or unreadable date counts as expired.
The helper signs a new server certificate with the stored CA key when the
SAN list differs or fewer than 180 days remain. It renews the CA
certificate with the same key and the same name when fewer than 365 days
remain, and then also signs a new server certificate. The earlier and the
renewed CA certificate both verify a server certificate of that key, so a
member copy of ca.crt stays valid until its own end date. The passwords
never change. tests/test-nats-tls-resign.sh replaces the lookup call below
with a fixture, so keep that call on one line in this form. The test also
holds the four day counts below.
*/}}
{{- define "varroa.nats.creds" -}}
{{- if not (hasKey $ "natsCreds") -}}
  {{- $secretName := printf "%s-nats-creds" $.Release.Name -}}
  {{- $existing := lookup "v1" "Secret" $.Release.Namespace $secretName -}}
  {{- $altNames := list (printf "%s-nats" $.Release.Name) (printf "%s-nats.%s.svc" $.Release.Name $.Release.Namespace) (printf "%s-nats.%s.svc.cluster.local" $.Release.Name $.Release.Namespace) (printf "*.%s-nats.%s.svc.cluster.local" $.Release.Name $.Release.Namespace) -}}
  {{- $external := .Values.nats.external | default dict -}}
  {{- if $external.host -}}
    {{- $altNames = append $altNames $external.host -}}
  {{- end -}}
  {{- range $external.extraSANs -}}
    {{- $altNames = append $altNames . -}}
  {{- end -}}
  {{- $ipSANs := $external.ipSANs | default list -}}
  {{- $sanRecord := printf "dns=%s;ip=%s" (join "," $altNames) (join "," $ipSANs) -}}
  {{- $layout := "2006-01-02T15:04:05Z07:00" -}}
  {{- $now := now -}}
  {{- $tlsDays := 365 -}}
  {{- $caDays := 3650 -}}
  {{- $tlsRenewSeconds := mul 180 86400 -}}
  {{- $caRenewSeconds := mul 365 86400 -}}
  {{- $tlsNotAfterNew := dateInZone $layout ($now | dateModify (printf "+%dh" (mul $tlsDays 24))) "UTC" -}}
  {{- $caNotAfterNew := dateInZone $layout ($now | dateModify (printf "+%dh" (mul $caDays 24))) "UTC" -}}
  {{- $caCN := printf "%s-nats-ca" $.Release.Name -}}
  {{- if $existing -}}
    {{- /*
      recorder-password is new. A release from before this change stores no
      such key, so `index` would return an empty string. Reuse the stored
      value when present, and generate one only the first time it appears.
    */ -}}
    {{- $recorderPass := randAlphaNum 32 -}}
    {{- if hasKey $existing.data "recorder-password" -}}
      {{- $recorderPass = index $existing.data "recorder-password" | b64dec -}}
    {{- end -}}
    {{- $agentPass := randAlphaNum 32 -}}
    {{- if hasKey $existing.data "agentrunner-password" -}}
      {{- $agentPass = index $existing.data "agentrunner-password" | b64dec -}}
    {{- end -}}
    {{- $tlsNotAfter := dig "metadata" "annotations" "varroa.dev/nats-tls-not-after" "" $existing -}}
    {{- $caNotAfter := dig "metadata" "annotations" "varroa.dev/nats-ca-not-after" "" $existing -}}
    {{- $caDue := lt (sub (toDate $layout $caNotAfter).Unix $now.Unix) $caRenewSeconds -}}
    {{- $sanStale := ne (dig "metadata" "annotations" "varroa.dev/nats-tls-sans" "" $existing) $sanRecord -}}
    {{- $tlsDue := lt (sub (toDate $layout $tlsNotAfter).Unix $now.Unix) $tlsRenewSeconds -}}
    {{- $caCert := index $existing.data "ca.crt" | b64dec -}}
    {{- $tlsCert := index $existing.data "tls.crt" | b64dec -}}
    {{- $tlsKey := index $existing.data "tls.key" | b64dec -}}
    {{- $caRenewed := false -}}
    {{- if or $caDue $sanStale $tlsDue -}}
      {{- if not (and (hasKey $existing.data "ca.crt") (hasKey $existing.data "ca.key")) -}}
        {{- fail (printf "Secret %s holds no ca.crt or ca.key, so the chart cannot sign a new NATS server certificate or renew the NATS CA." $secretName) -}}
      {{- end -}}
      {{- if $caDue -}}
        {{- $renewedCA := genCAWithKey $caCN $caDays (index $existing.data "ca.key" | b64dec) -}}
        {{- $caCert = $renewedCA.Cert -}}
        {{- $caNotAfter = $caNotAfterNew -}}
        {{- $caRenewed = true -}}
      {{- end -}}
      {{- $ca := buildCustomCert ($caCert | b64enc) (index $existing.data "ca.key") -}}
      {{- $cert := genSignedCert (printf "%s-nats" $.Release.Name) $ipSANs $altNames $tlsDays $ca -}}
      {{- $tlsCert = $cert.Cert -}}
      {{- $tlsKey = $cert.Key -}}
      {{- $tlsNotAfter = $tlsNotAfterNew -}}
    {{- end -}}
    {{- $_ := set $ "natsCreds" (dict
          "operator" (index $existing.data "operator-password" | b64dec)
          "gateway"  (index $existing.data "gateway-password"  | b64dec)
          "bff"      (index $existing.data "bff-password"       | b64dec)
          "recorder" $recorderPass
          "agentrunner" $agentPass
          "caCert"   $caCert
          "caKey"    (index $existing.data "ca.key" | b64dec)
          "tlsCert"  $tlsCert
          "tlsKey"   $tlsKey
          "tlsSANs"  $sanRecord
          "tlsNotAfter" $tlsNotAfter
          "caNotAfter" $caNotAfter
          "caRenewed" $caRenewed) -}}
  {{- else -}}
    {{- $ca := genCA $caCN $caDays -}}
    {{- $cert := genSignedCert (printf "%s-nats" $.Release.Name) $ipSANs $altNames $tlsDays $ca -}}
    {{- $_ := set $ "natsCreds" (dict
          "operator" (randAlphaNum 32)
          "gateway"  (randAlphaNum 32)
          "bff"      (randAlphaNum 32)
          "recorder" (randAlphaNum 32)
          "agentrunner" (randAlphaNum 32)
          "caCert"   $ca.Cert
          "caKey"    $ca.Key
          "tlsCert"  $cert.Cert
          "tlsKey"   $cert.Key
          "tlsSANs"  $sanRecord
          "tlsNotAfter" $tlsNotAfterNew
          "caNotAfter" $caNotAfterNew
          "caRenewed" false) -}}
  {{- end -}}
{{- end -}}
{{- end -}}

{{- define "varroa.nats.operatorPassword" -}}{{- include "varroa.nats.creds" $ -}}{{- $.natsCreds.operator -}}{{- end -}}
{{- define "varroa.nats.gatewayPassword" -}}{{- include "varroa.nats.creds" $ -}}{{- $.natsCreds.gateway -}}{{- end -}}
{{- define "varroa.nats.bffPassword"     -}}{{- include "varroa.nats.creds" $ -}}{{- $.natsCreds.bff     -}}{{- end -}}
{{- define "varroa.nats.recorderPassword" -}}{{- include "varroa.nats.creds" $ -}}{{- $.natsCreds.recorder -}}{{- end -}}
{{- define "varroa.nats.agentrunnerPassword" -}}{{- include "varroa.nats.creds" $ -}}{{- $.natsCreds.agentrunner -}}{{- end -}}
{{- define "varroa.nats.caCert"          -}}{{- include "varroa.nats.creds" $ -}}{{- $.natsCreds.caCert  -}}{{- end -}}
{{- define "varroa.nats.caKey"           -}}{{- include "varroa.nats.creds" $ -}}{{- $.natsCreds.caKey   -}}{{- end -}}
{{- define "varroa.nats.tlsCert"         -}}{{- include "varroa.nats.creds" $ -}}{{- $.natsCreds.tlsCert -}}{{- end -}}
{{- define "varroa.nats.tlsKey"          -}}{{- include "varroa.nats.creds" $ -}}{{- $.natsCreds.tlsKey  -}}{{- end -}}

{{/*
Record-store Postgres role password — stable-lookup, mirroring
varroa.nats.creds. Generates once per install and reuses the stored value
across an upgrade, so the recorder and client roles never lose access to
an existing database.
Usage: {{ include "varroa.recordStore.password" (dict "root" $ "role" "recorder") }}
*/}}
{{- define "varroa.recordStore.password" -}}
{{- $root := .root -}}
{{- $role := .role -}}
{{- $cacheKey := printf "recordStorePassword_%s" $role -}}
{{- if not (hasKey $root $cacheKey) -}}
  {{- $secretName := printf "varroa-recordstore-%s" $role -}}
  {{- $existing := lookup "v1" "Secret" $root.Release.Namespace $secretName -}}
  {{- if $existing -}}
    {{- $_ := set $root $cacheKey (index $existing.data "password" | b64dec) -}}
  {{- else -}}
    {{- $_ := set $root $cacheKey (randAlphaNum 32) -}}
  {{- end -}}
{{- end -}}
{{- index $root $cacheKey -}}
{{- end -}}

{{/*
Record-store Postgres DSN secretKeyRef name, for role "recorder" or
"client". cnpg mode uses the chart's own role Secret. External mode
points every role at the operator's single existingSecret.
Usage: {{ include "varroa.recordStore.dsnSecretName" (dict "root" $ "role" "recorder") }}
*/}}
{{- define "varroa.recordStore.dsnSecretName" -}}
{{- if eq .root.Values.recordStore.postgres.mode "external" -}}
{{- .root.Values.recordStore.postgres.external.existingSecret -}}
{{- else -}}
{{- printf "varroa-recordstore-%s" .role -}}
{{- end -}}
{{- end -}}

{{/*
Record-store Postgres DSN secretKeyRef key, mirroring
varroa.recordStore.dsnSecretName's mode split.
Usage: {{ include "varroa.recordStore.dsnSecretKey" $ }}
*/}}
{{- define "varroa.recordStore.dsnSecretKey" -}}
{{- if eq .Values.recordStore.postgres.mode "external" -}}
{{- .Values.recordStore.postgres.external.dsnKey -}}
{{- else -}}
dsn
{{- end -}}
{{- end -}}

{{/*
Record-store Postgres egress rule body, shared by the recorder, bff, and
gateway NetworkPolicy blocks. An empty cidrs list selects the in-cluster
CNPG pods in cnpg mode, and fails the render in external mode because no
selector can stand in for a pinned endpoint.
Usage: {{ include "varroa.np.recordStorePostgresEgress" $ | nindent 4 }}
*/}}
{{- define "varroa.np.recordStorePostgresEgress" -}}
{{- $toggle := .Values.networkPolicy.recordStorePostgresEgress -}}
{{- if $toggle.enabled -}}
- to:
{{- if $toggle.cidrs }}
{{- range $toggle.cidrs }}
  - ipBlock:
      cidr: {{ . }}
{{- end }}
{{- else if eq .Values.recordStore.postgres.mode "external" -}}
{{- fail "networkPolicy.recordStorePostgresEgress.cidrs must be set when recordStore.postgres.mode is \"external\" — pin the Postgres endpoint." -}}
{{- else }}
  - podSelector:
      matchLabels:
        cnpg.io/cluster: varroa-recordstore
{{- end }}
  ports:
{{- range $toggle.ports }}
  - protocol: TCP
    port: {{ . }}
{{- end }}
{{- end -}}
{{- end -}}

{{/*
Record-store blob egress rule body, shared by the recorder, bff, and
gateway NetworkPolicy blocks. Renders nothing in postgres blob mode,
because that path needs no external egress.
Usage: {{ include "varroa.np.recordStoreBlobEgress" $ | nindent 4 }}
*/}}
{{- define "varroa.np.recordStoreBlobEgress" -}}
{{- $toggle := .Values.networkPolicy.recordStoreBlobEgress -}}
{{- if and $toggle.enabled $toggle.cidrs (ne .Values.recordStore.blobs.mode "postgres") -}}
- to:
{{- range $toggle.cidrs }}
  - ipBlock:
      cidr: {{ . }}
{{- end }}
  ports:
{{- range $toggle.ports }}
  - protocol: TCP
    port: {{ . }}
{{- end }}
{{- end -}}
{{- end -}}

{{/*
Completion webhook egress rule for the recorder. Renders nothing unless
networkPolicy.recorderWebhookEgress.enabled is true and cidrs is non-empty.
The guard fails the render for enabled with an empty list.
Usage: {{ include "varroa.np.recorderWebhookEgress" $ | nindent 4 }}
*/}}
{{- define "varroa.np.recorderWebhookEgress" -}}
{{- $toggle := .Values.networkPolicy.recorderWebhookEgress -}}
{{- if and $toggle.enabled $toggle.cidrs -}}
- to:
{{- range $toggle.cidrs }}
  - ipBlock:
      cidr: {{ . }}
{{- end }}
  ports:
{{- range $toggle.ports }}
  - protocol: TCP
    port: {{ . }}
{{- end }}
{{- end -}}
{{- end -}}

{{/*
Pod placement (nodeSelector/tolerations/affinity) for a component.
Renders nothing when all three are unset/empty (the default), so a bare
`{{- include "varroa.podPlacement" .Values.<component> }}` call is a
byte-identical no-op against a render without it. Indentation is baked in
for the 6-space `spec.template.spec.*` level shared by every Deployment
this is wired into (operator, gateway, bff, dex) —
callers place it immediately before `containers:`.
Usage: {{- include "varroa.podPlacement" .Values.operator }}
*/}}
{{- define "varroa.podPlacement" -}}
{{- with .nodeSelector }}
      nodeSelector:
        {{- toYaml . | nindent 8 }}
{{- end }}
{{- with .tolerations }}
      tolerations:
        {{- toYaml . | nindent 8 }}
{{- end }}
{{- with .affinity }}
      affinity:
        {{- toYaml . | nindent 8 }}
{{- end }}
{{- end }}

{{/*
Hive-mode helper: returns "true" when .Values.mode is "hive".
*/}}
{{- define "varroa.isHive" -}}{{- if eq (.Values.mode | default "full") "hive" -}}true{{- end -}}{{- end -}}

{{/*
Local issuer: true when the BFF issues sessions that Jenkins controllers
verify. Each controller then fetches the BFF's JWKS from auth.bffUrl. The
auth-mode and local-accounts guards make auth.bffUrl a requirement here.
*/}}
{{- define "varroa.localIssuerInUse" -}}
{{- if and .Values.auth.bffUrl (or (eq .Values.auth.mode "local") (eq .Values.auth.mode "ldap") .Values.auth.localAccounts.enabled) -}}true{{- end -}}
{{- end -}}

{{/*
varroa.busURL gives the NATS bus URL of every bus client: bus.url, else the
in-cluster default nats://<release>-nats:4222. No component has its own bus
URL value, because the core NATS egress rule reads only bus.url.
Usage: {{ include "varroa.busURL" $ }}
*/}}
{{- define "varroa.busURL" -}}
{{- .Values.bus.url | default (printf "nats://%s-nats:4222" .Release.Name) -}}
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

{{- /*
varroa.imagePullSecrets renders the pod-level imagePullSecrets list from
global.image.pullSecretNames. The NATS subchart reads the same global key,
so one value covers every pod of the release. Renders nothing when the
list is empty. varroa.pullSecretNames answers the names joined by commas,
and fails the render on a value that is not a list of strings.
Usage: {{- include "varroa.imagePullSecrets" . | nindent 6 }}
*/}}
{{- define "varroa.pullSecretNames" -}}
{{- $names := dig "image" "pullSecretNames" list (.Values.global | default dict) -}}
{{- if not (kindIs "slice" $names) -}}
{{- fail "global.image.pullSecretNames must be a list of Secret names" -}}
{{- end -}}
{{- range $names -}}
{{- if not (kindIs "string" .) -}}
{{- fail (printf "global.image.pullSecretNames must hold Secret names as strings, not %v" .) -}}
{{- end -}}
{{- end -}}
{{- join "," $names -}}
{{- end -}}

{{- define "varroa.imagePullSecrets" -}}
{{- $names := splitList "," (include "varroa.pullSecretNames" .) | compact -}}
{{- with $names }}
imagePullSecrets:
{{- range . }}
  - name: {{ . | quote }}
{{- end }}
{{- end }}
{{- end -}}
