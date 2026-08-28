#!/usr/bin/env bash
# localdev.sh — reproducible local dev environment for Varroa on kind.
#
# Phases:
#   up          (default) full converge: cluster, ingress, certs, DNS, images,
#               helm release, sample controller, summary
#   images      rebuild + kind-load images, roll the release to the new tags
#   controller  (re-)apply the sample workload against an existing environment
#   down        delete the kind cluster (keeps .localdev/ certs)
#
# Env flags:
#   LOCALDEV_SKIP_CONTROLLER=1   skip the sample controller in `up`
#   LOCALDEV_OFFLINE=1           offline mode: no pull-through, use fixture, skip controller
#
# Everything is idempotent: re-running `up` converges instead of recreating.
# Fallback note (design D6): if the in-cluster smart-HTTP git server
# (hack/localdev/git-server/) misbehaves, point the ComposedBundle at the
# public https://github.com/varroaci/varroa-jenkins-controllers.git
# (path bundle-test).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT"

CLUSTER=varroa-localdev
CTX=kind-varroa-localdev
NS=varroa-system
WORKLOAD_NS=varroa
DOMAIN=varroa.localtest.me
RELEASE=varroa
INGRESS_NGINX_MANIFEST="https://raw.githubusercontent.com/kubernetes/ingress-nginx/controller-v1.13.1/deploy/static/provider/kind/deploy.yaml"
LOCALDEV_DIR="$REPO_ROOT/.localdev"
KIND_CONFIG="$REPO_ROOT/hack/localdev/kind-config.yaml"
BUNDLE_DIR="$REPO_ROOT/hack/localdev/bundle"
MANIFEST_DIR="$REPO_ROOT/hack/localdev/manifests"

K() { kubectl --context "$CTX" "$@"; }

log()  { echo ">>> $*"; }
die()  { echo "ERROR: $*" >&2; exit 1; }

# Bounded wait: wait_for "<description>" <attempts> <sleep-seconds> <command...>
wait_for() {
  local desc=$1 attempts=$2 pause=$3
  shift 3
  for _ in $(seq 1 "$attempts"); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep "$pause"
  done
  echo "WARN: timed out waiting for: $desc" >&2
  return 1
}

cluster_exists() { kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; }

release_exists() { helm --kube-context "$CTX" status "$RELEASE" -n "$NS" >/dev/null 2>&1; }

# ---------------------------------------------------------------------------
# preflight
# ---------------------------------------------------------------------------
preflight() {
  local missing=0
  for bin in docker kind kubectl helm git envsubst; do
    command -v "$bin" >/dev/null || { echo "ERROR: '$bin' not found on PATH — install it first"; missing=1; }
  done
  [ "$missing" -eq 0 ] || exit 1
  docker info >/dev/null 2>&1 || die "docker daemon is not responding — start Docker"
  # Ports 80/443 must be free unless our own kind node already owns them.
  if ! cluster_exists; then
    for port in 80 443; do
      if command -v ss >/dev/null; then
        busy=$(ss -ltn "( sport = :$port )" 2>/dev/null | grep -c LISTEN || true)
      else
        # No ss (minimal environments): probe with bash's /dev/tcp instead.
        busy=0
        (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null && busy=1
      fi
      if [ "$busy" != "0" ]; then
        die "host port $port is busy — stop whatever listens on it (ss -ltnp 'sport = :$port') or 'make localdev-down' a stale cluster"
      fi
    done
  fi
}

# ---------------------------------------------------------------------------
# cluster + ingress + certs + dns
# ---------------------------------------------------------------------------
ensure_cluster() {
  if cluster_exists; then
    log "kind cluster $CLUSTER exists"
  else
    log "creating kind cluster $CLUSTER"
    kind create cluster --config "$KIND_CONFIG"
  fi
}

ensure_ingress() {
  log "applying ingress-nginx (pinned)"
  K apply -f "$INGRESS_NGINX_MANIFEST" >/dev/null
  wait_for "ingress-nginx controller rollout" 60 5 \
    bash -c "kubectl --context $CTX -n ingress-nginx rollout status deployment/ingress-nginx-controller --timeout=5s" \
    || die "ingress-nginx did not become ready"
}

ensure_certs() {
  mkdir -p "$LOCALDEV_DIR"
  local crt="$LOCALDEV_DIR/tls.crt" key="$LOCALDEV_DIR/tls.key"
  if [ ! -f "$crt" ] || [ ! -f "$key" ]; then
    if command -v mkcert >/dev/null; then
      log "generating TLS cert with mkcert (*.$DOMAIN)"
      mkcert -cert-file "$crt" -key-file "$key" "*.$DOMAIN" "$DOMAIN"
    else
      log "mkcert not found — generating self-signed cert (browser will warn)"
      openssl req -x509 -newkey rsa:2048 -nodes -days 825 \
        -keyout "$key" -out "$crt" \
        -subj "/CN=*.$DOMAIN" \
        -addext "subjectAltName=DNS:*.$DOMAIN,DNS:$DOMAIN" >/dev/null 2>&1
    fi
  else
    log "reusing TLS cert from .localdev/"
  fi
  K create namespace "$NS" --dry-run=client -o yaml | K apply -f - >/dev/null
  K -n "$NS" create secret tls varroa-localdev-tls --cert="$crt" --key="$key" \
    --dry-run=client -o yaml | K apply -f - >/dev/null
}

ensure_coredns() {
  if K -n kube-system get configmap coredns -o jsonpath='{.data.Corefile}' | grep -q "$DOMAIN"; then
    log "CoreDNS rewrite already present"
    return
  fi
  log "adding CoreDNS rewrite for *.$DOMAIN -> ingress-nginx"
  local corefile rewrite
  corefile=$(K -n kube-system get configmap coredns -o jsonpath='{.data.Corefile}')
  # shellcheck disable=SC2016
  rewrite='    rewrite name regex (.*)\.varroa\.localtest\.me ingress-nginx-controller.ingress-nginx.svc.cluster.local answer auto'
  corefile=$(printf '%s\n' "$corefile" | awk -v r="$rewrite" '{print} /^ *ready *$/{print r}')
  echo "$corefile" | grep -q "$DOMAIN" || die "failed to insert CoreDNS rewrite (Corefile shape changed?)"
  # Merge-patch only the Corefile key — never recreate the ConfigMap, which
  # would drop any other data keys (e.g. NodeHosts on some distributions).
  # kubectl dry-run builds the JSON-escaped payload; merge patch leaves other
  # data keys intact.
  K -n kube-system patch configmap coredns --type merge \
    -p "$(kubectl create configmap coredns --from-literal=Corefile="$corefile" --dry-run=client -o json)" >/dev/null
  K -n kube-system rollout restart deployment/coredns >/dev/null
  wait_for "coredns rollout" 24 5 \
    bash -c "kubectl --context $CTX -n kube-system rollout status deployment/coredns --timeout=5s" \
    || die "coredns did not come back after the rewrite patch"
}

# ---------------------------------------------------------------------------
# images
# ---------------------------------------------------------------------------
image_id12() { docker image inspect --format '{{.Id}}' "$1" | cut -d: -f2 | cut -c1-12; }

build_images() {
  log "building backend image (operator/gateway/bff/mite + plugin HPI)"
  docker build --provenance=false --sbom=false -t varroa-jenkins:build "$REPO_ROOT" >/dev/null
  log "building frontend image"
  docker build --provenance=false --sbom=false -t varroa-frontend:build "$REPO_ROOT/frontend" >/dev/null
  log "building localdev git-server image"
  docker build --provenance=false --sbom=false -t varroa-localdev-git:build "$REPO_ROOT/hack/localdev/git-server" >/dev/null
  BACKEND_TAG="dev-$(image_id12 varroa-jenkins:build)"
  FRONTEND_TAG="dev-$(image_id12 varroa-frontend:build)"
  GIT_TAG="dev-$(image_id12 varroa-localdev-git:build)"
  docker tag varroa-jenkins:build "varroa-jenkins:$BACKEND_TAG"
  docker tag varroa-frontend:build "varroa-frontend:$FRONTEND_TAG"
  docker tag varroa-localdev-git:build "varroa-localdev-git:$GIT_TAG"
  log "loading images into kind ($BACKEND_TAG / $FRONTEND_TAG / git $GIT_TAG)"
  kind load docker-image "varroa-jenkins:$BACKEND_TAG" "varroa-frontend:$FRONTEND_TAG" \
    "varroa-localdev-git:$GIT_TAG" --name "$CLUSTER"
  MITE_IMAGE="docker.io/library/varroa-jenkins:$BACKEND_TAG"
  GIT_IMAGE="docker.io/library/varroa-localdev-git:$GIT_TAG"
}

helm_deploy() {
  # charts/varroa/charts/ is gitignored — fetch the NATS subchart (pinned by
  # Chart.lock) on a fresh clone. helm v3 needs the repo registered first.
  if ! ls "$REPO_ROOT"/charts/varroa/charts/nats-*.tgz >/dev/null 2>&1; then
    log "fetching chart dependencies (helm dependency build)"
    helm repo add nats https://nats-io.github.io/k8s/helm/charts/ --force-update >/dev/null
    helm dependency build "$REPO_ROOT/charts/varroa" >/dev/null
  fi
  log "applying CRDs"
  K apply -f "$REPO_ROOT/charts/varroa/crds/" >/dev/null
  log "helm upgrade --install $RELEASE (tags: backend=$BACKEND_TAG frontend=$FRONTEND_TAG)"
  # --force-conflicts: helm v4 server-side apply vs the HPA controller, which
  # co-owns .spec.replicas on bff/gateway via the scale subresource.
  helm --kube-context "$CTX" upgrade --install "$RELEASE" "$REPO_ROOT/charts/varroa" \
    -n "$NS" --create-namespace --force-conflicts \
    -f "$REPO_ROOT/charts/varroa/values-localdev.yaml" \
    --set operator.image.tag="$BACKEND_TAG" \
    --set gateway.image.tag="$BACKEND_TAG" \
    --set bff.image.tag="$BACKEND_TAG" \
    --set frontend.image.tag="$FRONTEND_TAG" \
    --set operator.miteImage="$MITE_IMAGE" \
    --set updateCenter.enabled=true \
    --set updateCenter.storage.type=local \
    --set updateCenter.image.repository=varroa-jenkins \
    --set updateCenter.image.tag="$BACKEND_TAG" \
    --set updateCenter.importToken=localdev-import-token \
    ${LOCALDEV_OFFLINE:+--set updateCenter.pullThrough.enabled=false} >/dev/null
  for d in "$RELEASE-varroa-operator" "$RELEASE-varroa-gateway" "$RELEASE-varroa-bff" "$RELEASE-frontend" "$RELEASE-updatecenter"; do
    wait_for "deployment $d rollout" 60 5 \
      bash -c "kubectl --context $CTX -n $NS rollout status deployment/$d --timeout=5s" \
      || die "deployment $d did not become ready (kubectl --context $CTX -n $NS describe deploy $d)"
  done
  wait_for "nats statefulset rollout" 60 5 \
    bash -c "kubectl --context $CTX -n $NS rollout status statefulset/$RELEASE-nats --timeout=5s" \
    || die "NATS did not become ready"
  K apply -f "$MANIFEST_DIR/admin-user.yaml" >/dev/null
  K apply -f "$MANIFEST_DIR/admin-rolebinding.yaml" >/dev/null
}

# ---------------------------------------------------------------------------
# seed updatecenter
# ---------------------------------------------------------------------------
seed_updatecenter() {
  mkdir -p bin
  local container_id
  container_id=$(docker create "varroa-jenkins:$BACKEND_TAG")
  docker cp "$container_id:/app/varroactl" bin/varroactl
  docker rm "$container_id" >/dev/null
  chmod +x bin/varroactl

  K port-forward -n "$NS" svc/varroa-updatecenter 8080:8080 &
  local pf_pid=$!
  # A bash RETURN trap set in a function leaks to callers' returns, where this
  # function's local pf_pid is already gone — reference it set-u-safely so the
  # leaked trap can't abort the whole run with "pf_pid: unbound variable".
  trap 'kill "${pf_pid:-}" 2>/dev/null || true' RETURN

  # Bounded readiness poll against localhost:8080 (not a bare sleep)
  wait_for "port-forward to varroa-updatecenter:8080" 12 5 \
    bash -c "command -v curl >/dev/null && curl -sf http://localhost:8080/healthz >/dev/null 2>&1" \
    || echo "WARN: port-forward readiness check timed out, proceeding anyway" >&2

  local token
  token=$(K get secret varroa-updatecenter-import-token -n "$NS" \
    -o jsonpath='{.data.token}' | base64 -d)
  export VARROACTL_UC_TOKEN="$token"

  if [[ "${LOCALDEV_OFFLINE:-}" == "1" ]]; then
    if ! bin/varroactl import --from dir://hack/localdev/pluginpack-fixture \
        --to uc://localhost:8080; then
      echo "ERROR: offline seed import failed (LOCALDEV_OFFLINE=1, no pull-through fallback)" >&2
      return 1
    fi
  else
    if ! bin/varroactl import --from "oci://ghcr.io/varroaci/varroa-jenkins/plugin-pack:jenkins-version-2-555" \
        --to uc://localhost:8080; then
      echo "WARNING: seed pack import failed, continuing — sample controller will rely on pull-through" >&2
    fi
  fi
}

phase_images() {
  cluster_exists || die "no $CLUSTER cluster — run 'make localdev' first"
  release_exists || die "no '$RELEASE' helm release in $NS — run 'make localdev' first"
  build_images
  helm_deploy
  log "images rolled (backend=$BACKEND_TAG frontend=$FRONTEND_TAG)"
}

# ---------------------------------------------------------------------------
# sample controller
# ---------------------------------------------------------------------------
phase_controller() {
  cluster_exists || die "no $CLUSTER cluster — run 'make localdev' first"
  release_exists || die "no '$RELEASE' helm release in $NS — run 'make localdev' first"
  if [ -z "${MITE_IMAGE:-}" ]; then
    # Standalone invocation: reuse the tag the release currently runs.
    MITE_IMAGE=$(K -n "$NS" get deployment "$RELEASE-varroa-operator" \
      -o jsonpath='{.spec.template.spec.containers[0].image}')
    MITE_IMAGE="docker.io/library/$MITE_IMAGE"
  fi
  if [ -z "${GIT_IMAGE:-}" ]; then
    # Standalone invocation: build + load just the tiny git-server image.
    docker build --provenance=false --sbom=false -t varroa-localdev-git:build "$REPO_ROOT/hack/localdev/git-server" >/dev/null
    GIT_TAG="dev-$(image_id12 varroa-localdev-git:build)"
    docker tag varroa-localdev-git:build "varroa-localdev-git:$GIT_TAG"
    kind load docker-image "varroa-localdev-git:$GIT_TAG" --name "$CLUSTER"
    GIT_IMAGE="docker.io/library/varroa-localdev-git:$GIT_TAG"
  fi

  log "applying sample workload (ns $WORKLOAD_NS)"
  K create namespace "$WORKLOAD_NS" --dry-run=client -o yaml | K apply -f - >/dev/null
  K -n "$WORKLOAD_NS" create secret tls getting-started-tls \
    --cert="$LOCALDEV_DIR/tls.crt" --key="$LOCALDEV_DIR/tls.key" \
    --dry-run=client -o yaml | K apply -f - >/dev/null
  K -n "$NS" create configmap localdev-bundle --from-file="$BUNDLE_DIR" \
    --dry-run=client -o yaml | K apply -f - >/dev/null

  LOCALDEV_BUNDLE_HASH=$(cat "$BUNDLE_DIR"/* | sha256sum | cut -c1-12)
  export LOCALDEV_BUNDLE_HASH LOCALDEV_GIT_IMAGE="$GIT_IMAGE"
  # shellcheck disable=SC2016 # literal var names are envsubst's whitelist syntax
  envsubst '$LOCALDEV_BUNDLE_HASH $LOCALDEV_GIT_IMAGE' < "$MANIFEST_DIR/git-server.yaml" | K apply -f - >/dev/null
  wait_for "localdev-git rollout" 24 5 \
    bash -c "kubectl --context $CTX -n $NS rollout status deployment/localdev-git --timeout=5s" \
    || die "localdev git server did not become ready"
  K apply -f "$MANIFEST_DIR/sample-controller.yaml" >/dev/null

  # Non-fatal bounded waits (~10 min total): the control plane stays usable
  # even if Jenkins is slow to pull/boot.
  local ok=1
  wait_for "ComposedBundle getting-started Ready" 24 5 \
    bash -c "kubectl --context $CTX -n $WORKLOAD_NS get composedbundle getting-started -o jsonpath='{.status.phase}' | grep -qx Ready" || ok=0
  if [ "$ok" -eq 1 ]; then
    wait_for "Controller getting-started Running/Connected" 60 5 \
      bash -c "kubectl --context $CTX -n $WORKLOAD_NS get controller getting-started -o jsonpath='{.status.phase}' | grep -Eqx 'Running|Connected'" || ok=0
  fi
  if [ "$ok" -eq 1 ]; then
    # The StatefulSet name carries a UID-derived prefix (<name>-<uid8>-jenkins)
    # — discover it instead of guessing.
    # Generous budget: a cold cluster pulls the Jenkins image + plugins (~8 min).
    wait_for "sample Jenkins StatefulSet ready" 96 5 \
      bash -c "kubectl --context $CTX -n $WORKLOAD_NS get sts -o jsonpath='{.items[?(@.status.readyReplicas==1)].metadata.name}' | grep -q jenkins" || ok=0
  fi
  if [ "$ok" -ne 1 ]; then
    echo "WARN: the sample controller is not ready yet. The control plane is still usable." >&2
    echo "      Diagnose with:" >&2
    echo "        kubectl --context $CTX -n $WORKLOAD_NS describe controller getting-started" >&2
    echo "        kubectl --context $CTX -n $WORKLOAD_NS get pods" >&2
    echo "        kubectl --context $CTX -n $NS logs deploy/$RELEASE-varroa-operator --tail=100" >&2
  fi
}

# ---------------------------------------------------------------------------
# summary / down
# ---------------------------------------------------------------------------
summary() {
  cat <<EOF

============================================================
 Varroa localdev is up
------------------------------------------------------------
 Dashboard   https://app.$DOMAIN
 Login       admin / password
 Bundle git  https://git.$DOMAIN/cgi-bin/git/localdev-bundle.git
EOF
  if [ "${LOCALDEV_SKIP_CONTROLLER:-0}" != "1" ]; then
    echo " Jenkins     https://getting-started.$DOMAIN"
  fi
  cat <<EOF
------------------------------------------------------------
 Iterate     make localdev-images     (rebuild + roll)
 Sample      make localdev-controller (re-apply workload)
 Teardown    make localdev-down       (deletes all cluster data)
============================================================
EOF
}

phase_up() {
  preflight
  ensure_cluster
  ensure_ingress
  ensure_certs
  ensure_coredns
  build_images
  helm_deploy
  seed_updatecenter
  if [[ "${LOCALDEV_OFFLINE:-}" == "1" ]] && [[ "${LOCALDEV_SKIP_CONTROLLER:-0}" != "1" ]]; then
    log "LOCALDEV_OFFLINE=1 — implying LOCALDEV_SKIP_CONTROLLER=1 (no network-reachable plugin closure for the sample controller)"
    LOCALDEV_SKIP_CONTROLLER=1
  fi
  if [ "${LOCALDEV_SKIP_CONTROLLER:-0}" = "1" ]; then
    log "LOCALDEV_SKIP_CONTROLLER=1 — skipping sample controller"
  else
    phase_controller
  fi
  summary
}

phase_down() {
  if cluster_exists; then
    kind delete cluster --name "$CLUSTER"
  else
    log "no $CLUSTER cluster — nothing to do"
  fi
}

case "${1:-up}" in
  up)         phase_up ;;
  images)     preflight; phase_images ;;
  seed)       preflight; build_images; helm_deploy; seed_updatecenter ;;
  controller) preflight; phase_controller ;;
  down)       phase_down ;;
  *)          die "unknown phase '$1' (expected: up|images|seed|controller|down)" ;;
esac
