#!/usr/bin/env bash
# Cutover for the rename of Team to Tenant (design decision 10).
#
# The script runs nine steps on the core cluster and on each member
# cluster. Each step checks its end state first, so a rerun resumes
# after the last finished step. Step 6 blocks until the operator
# Deployment reports --image. The supervisor runs the deploy script
# from a second shell while step 6 waits.
#
# Usage:
#   hack/migrate-team-to-tenant.sh --core <ctx> [--member <ctx>]... \
#     --dsn <record-store dsn> --image <image ref> \
#     --forge-token-file <path> [--dry-run]
#
# Environment:
#   VARROA_FORGE_URL        Forgejo base URL for step 8.
#   VARROA_NAMESPACE        Operator namespace. Default: varroa-system.
#   VARROA_OPERATOR_DEPLOY  Operator Deployment name. Default: varroa-varroa-operator.
#   VARROA_NATS_BOX         NATS box Deployment name. Default: varroa-nats-box.
#   VARROA_DEPLOY_TIMEOUT   Seconds that step 6 waits. Default: 3600.
#
# --dry-run reads the clusters and the forge. It writes nothing. It
# prints each write it would make, and it does not contact the record
# store. Run it before the cutover window.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
NS="${VARROA_NAMESPACE:-varroa-system}"
DEPLOY="${VARROA_OPERATOR_DEPLOY:-varroa-varroa-operator}"
NATS_BOX="${VARROA_NATS_BOX:-varroa-nats-box}"
DEPLOY_TIMEOUT="${VARROA_DEPLOY_TIMEOUT:-3600}"
FORGE_URL="${VARROA_FORGE_URL:-}"
OLD_LABEL_KEYS=(varroa.dev/team-name varroa.dev/team-agent varroa.dev/team-agent-name varroa.dev/team-agent-namespace)

# SQL. The psql variables c (cluster), t (tenant), and u (uid) come in by -v.
SQL_PLACEMENT_COUNT="SELECT count(*) FROM varroa.tenant_placements WHERE cluster = :'c' AND tenant = :'t';"
SQL_RUN_COUNT="SELECT count(*) FROM varroa.runs WHERE cluster = :'c' AND tenant = :'t';"
# One transaction per tenant, so the dispatcher never sees a half-stamped fence.
SQL_FENCE_BASE="BEGIN;
UPDATE varroa.tenant_placements SET incarnation = :'u' WHERE cluster = :'c' AND tenant = :'t';
UPDATE varroa.webhook_deliveries SET incarnation = :'u' WHERE cluster = :'c' AND tenant = :'t' AND state = 'pending';
COMMIT;"
SQL_FENCE_FULL="BEGIN;
UPDATE varroa.tenant_placements SET incarnation = :'u' WHERE cluster = :'c' AND tenant = :'t';
UPDATE varroa.webhook_deliveries SET incarnation = :'u' WHERE cluster = :'c' AND tenant = :'t' AND state = 'pending';
UPDATE varroa.run_intents SET incarnation = :'u' WHERE cluster = :'c' AND tenant = :'t' AND incarnation = :'o';
COMMIT;"

CORE=""
MEMBERS=()
DSN=""
IMAGE=""
FORGE_TOKEN_FILE=""
DRY=0
FAILS=0
WORK=""

log() { printf '[%s] %s\n' "$(date +%H:%M:%S)" "$*"; }
die() { log "ERROR: $*" >&2; exit 1; }
fail() { log "FAIL: $*" >&2; FAILS=$((FAILS + 1)); }

usage() {
	sed -n '2,20p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

while [[ $# -gt 0 ]]; do
	case "$1" in
	--core) CORE="$2"; shift 2 ;;
	--member) MEMBERS+=("$2"); shift 2 ;;
	--dsn) DSN="$2"; shift 2 ;;
	--image) IMAGE="$2"; shift 2 ;;
	--forge-token-file) FORGE_TOKEN_FILE="$2"; shift 2 ;;
	--dry-run) DRY=1; shift ;;
	-h | --help) usage; exit 0 ;;
	*) die "unknown argument: $1" ;;
	esac
done

[[ -n "$CORE" ]] || die "--core is required"
if ((DRY == 0)); then
	[[ -n "$DSN" && -n "$IMAGE" && -n "$FORGE_TOKEN_FILE" ]] ||
		die "--dsn, --image, and --forge-token-file are required without --dry-run"
	[[ -n "$FORGE_URL" ]] || die "VARROA_FORGE_URL is required without --dry-run"
	[[ -r "$FORGE_TOKEN_FILE" ]] || die "cannot read $FORGE_TOKEN_FILE"
	command -v psql >/dev/null || die "psql is required"
fi

CONTEXTS=("$CORE")
if ((${#MEMBERS[@]} > 0)); then
	CONTEXTS+=("${MEMBERS[@]}")
fi

# kc runs kubectl against one context.
kc() {
	local ctx="$1"
	shift
	kubectl --context "$ctx" "$@"
}

# act runs a write, or prints it in dry-run mode.
act() {
	if ((DRY)); then
		log "DRY-RUN: $*"
	else
		"$@"
	fi
}

# list_kind prints the list of one varroa.dev resource, or an empty list
# when its CRD is absent.
list_kind() {
	local ctx="$1" res="$2"
	if kc "$ctx" get crd "$res.varroa.dev" >/dev/null 2>&1; then
		kc "$ctx" get "$res.varroa.dev" -A -o json
	else
		printf '{"items":[]}\n'
	fi
}

cluster_name() {
	kc "$1" -n "$NS" get deploy "$DEPLOY" -o json |
		jq -r '.spec.template.spec.containers[] | select(.name == "operator") | .env[]? | select(.name == "VARROA_CLUSTER_NAME") | .value'
}

# db_sql runs one statement from stdin-free arguments. The first argument
# is the SQL, the rest are psql -v options. In dry-run it prints the
# statement and runs nothing, so the dry run never contacts the record store.
db_sql() {
	local sql="$1"
	shift
	if ((DRY)); then
		log "DRY-RUN: psql: $sql" >&2
		echo "n/a"
		return 0
	fi
	printf '%s\n' "$sql" | psql "$DSN" -X -At -v ON_ERROR_STOP=1 "$@" -f -
}

# valid_name rejects any value that is not a plain Kubernetes name.
valid_name() {
	[[ "$1" =~ ^[A-Za-z0-9._-]+$ ]] || die "unsafe name from the cluster: $1"
}

# write_filters writes the jq filters that build each apply file.
write_filters() {
	cat >"$WORK/convert.jq" <<'JQ'
def convert($k):
  del(.metadata.uid, .metadata.resourceVersion, .metadata.creationTimestamp,
      .metadata.managedFields, .metadata.finalizers, .metadata.ownerReferences,
      .metadata.generation, .metadata.selfLink, .metadata.deletionTimestamp,
      .metadata.annotations["kubectl.kubernetes.io/last-applied-configuration"],
      .status)
  | .kind = $k
  | if .spec.teamRef != null then .spec.tenantRef = .spec.teamRef | del(.spec.teamRef) else . end;
{apiVersion: "v1", kind: "List", items: [.items[] | convert($k)]}
JQ
	cat >"$WORK/rbac.jq" <<'JQ'
def tenantize:
  if type == "string" then
    gsub("/teams/"; "/tenants/") | gsub("teams:"; "tenants:") | gsub("Group:team-"; "Group:tenant-")
  else . end;
{apiVersion: "v1", kind: "List", items: [
  .items[]
  | select((.metadata.labels["varroa.dev/managed-by"] // "") != "team")
  | if .spec then .spec |= walk(tenantize) else . end
  | if .metadata.labels then
      .metadata.labels |= with_entries(
        if (.key | test("managed-by|federated")) and .value == "team" then .value = "tenant" else . end)
    else . end
  | del(.metadata.uid, .metadata.resourceVersion, .metadata.creationTimestamp,
        .metadata.managedFields, .metadata.generation, .metadata.selfLink,
        .metadata.annotations["kubectl.kubernetes.io/last-applied-configuration"], .status)
]}
JQ
	cat >"$WORK/runner.jq" <<'JQ'
{apiVersion: "v1", kind: "List", items: [
  .items[]
  | if .spec.team != null then .spec.tenant = .spec.team | del(.spec.team) else . end
  | del(.metadata.uid, .metadata.resourceVersion, .metadata.creationTimestamp,
        .metadata.managedFields, .metadata.generation, .metadata.selfLink,
        .metadata.annotations["kubectl.kubernetes.io/last-applied-configuration"], .status)
]}
JQ
}

# step 1: the Tenant and TenantAgent CRDs. The old operator ignores them.
step1_crds() {
	local ctx="$1" pair name file
	log "step 1 [$ctx]: Tenant and TenantAgent CRDs"
	for pair in tenants.varroa.dev:varroa.dev_tenants.yaml tenantagents.varroa.dev:varroa.dev_tenantagents.yaml; do
		name="${pair%%:*}"
		file="${pair#*:}"
		if kc "$ctx" get crd "$name" >/dev/null 2>&1; then
			log "  $name already present"
		else
			act kubectl --context "$ctx" apply -f "$REPO_ROOT/charts/varroa/crds/$file"
		fi
	done
}

# step 2: copy each Team and TeamAgent to a Tenant or TenantAgent, then
# copy the status subresource so cells.Place keeps the same cell.
step2_objects() {
	local ctx="$1" dir cluster line res ns name status n_t n_a ns_args
	dir="$WORK/$ctx"
	mkdir -p "$dir"
	cluster="$(cluster_name "$ctx")"
	[[ -n "$cluster" ]] || die "no VARROA_CLUSTER_NAME on $ctx"
	printf '%s\n' "$cluster" >"$dir/cluster"
	log "step 2 [$ctx]: copy Team and TeamAgent objects (cluster $cluster)"

	list_kind "$ctx" teams >"$dir/old-teams.json"
	list_kind "$ctx" teamagents >"$dir/old-teamagents.json"
	n_t=$(jq '.items | length' "$dir/old-teams.json")
	n_a=$(jq '.items | length' "$dir/old-teamagents.json")
	log "  found $n_t Team and $n_a TeamAgent"

	jq -f "$WORK/convert.jq" --arg k Tenant "$dir/old-teams.json" >"$dir/tenants.json"
	jq -f "$WORK/convert.jq" --arg k TenantAgent "$dir/old-teamagents.json" >"$dir/tenantagents.json"
	jq -c --arg r tenants.varroa.dev \
		'.items[] | {res: $r, namespace: (.metadata.namespace // ""), name: .metadata.name, status: (.status // {})}' \
		"$dir/old-teams.json" >"$dir/statuses.jsonl"
	jq -c --arg r tenantagents.varroa.dev \
		'.items[] | {res: $r, namespace: (.metadata.namespace // ""), name: .metadata.name, status: (.status // {})}' \
		"$dir/old-teamagents.json" >>"$dir/statuses.jsonl"

	if ((n_t > 0)); then act kubectl --context "$ctx" apply -f "$dir/tenants.json"; fi
	if ((n_a > 0)); then act kubectl --context "$ctx" apply -f "$dir/tenantagents.json"; fi

	# Copy the status of each object so status.placement carries over.
	while IFS= read -r line; do
		res="$(jq -r .res <<<"$line")"
		ns="$(jq -r .namespace <<<"$line")"
		name="$(jq -r .name <<<"$line")"
		status="$(jq -c '.status' <<<"$line")"
		if [[ "$status" == "{}" ]]; then continue; fi
		valid_name "$name"
		ns_args=()
		if [[ -n "$ns" ]]; then ns_args=(-n "$ns"); fi
		act kubectl --context "$ctx" ${ns_args[@]+"${ns_args[@]}"} patch "$res" "$name" --subresource=status \
			--type merge -p "{\"status\":$status}"
	done <"$dir/statuses.jsonl"
}

# step 3: rewrite grants and group subjects on objects the operator
# does not own. Owned Groups and bindings are replaced in step 7.
step3_rbac() {
	local ctx="$1" dir res n
	dir="$WORK/$ctx"
	for res in varroaroles varroarolebindings jenkinsrolebindings groups; do
		list_kind "$ctx" "$res" >"$dir/rbac-$res.json"
	done
	jq -s '{items: [.[].items[]]}' "$dir"/rbac-varroaroles.json "$dir"/rbac-varroarolebindings.json \
		"$dir"/rbac-jenkinsrolebindings.json "$dir"/rbac-groups.json >"$dir/rbac-all.json"
	jq -f "$WORK/rbac.jq" "$dir/rbac-all.json" >"$dir/rbac-rewrite.json"
	n=$(jq '.items | length' "$dir/rbac-rewrite.json")
	log "step 3 [$ctx]: rewrite $n RBAC objects"
	if ((n > 0)); then act kubectl --context "$ctx" apply -f "$dir/rbac-rewrite.json"; fi
}

# step 4: stop the old operator, then apply the renamed Runner CRD and
# rewrite each Runner from the export taken before the apply.
step4_stop_operator() {
	local ctx="$1" dir replicas sel n
	dir="$WORK/$ctx"
	list_kind "$ctx" runners >"$dir/runners-old.json"
	replicas="$(kc "$ctx" -n "$NS" get deploy "$DEPLOY" -o jsonpath='{.spec.replicas}')"
	log "step 4 [$ctx]: scale $DEPLOY from $replicas to 0"
	if [[ "$replicas" != "0" ]]; then
		act kubectl --context "$ctx" -n "$NS" scale deploy "$DEPLOY" --replicas=0
	fi
	sel="$(kc "$ctx" -n "$NS" get deploy "$DEPLOY" -o json |
		jq -r '.spec.selector.matchLabels | to_entries | map("\(.key)=\(.value)") | join(",")')"
	if [[ -n "$(kc "$ctx" -n "$NS" get pods -l "$sel" -o name)" ]]; then
		act kubectl --context "$ctx" -n "$NS" wait --for=delete pod -l "$sel" --timeout=300s
	fi
	act kubectl --context "$ctx" apply -f "$REPO_ROOT/charts/varroa/crds/varroa.dev_runners.yaml"
	jq -f "$WORK/runner.jq" "$dir/runners-old.json" >"$dir/runners-rewrite.json"
	n=$(jq '.items | length' "$dir/runners-old.json")
	log "  rewrite $n Runner objects"
	if ((n > 0)); then act kubectl --context "$ctx" apply -f "$dir/runners-rewrite.json"; fi
}

# step 5: take the before counts, then restamp the uid fences with the
# new Tenant uid. The old operator must be stopped first (step 4).
step5_fences() {
	local ctx c t u o
	: >"$WORK/pairs.tsv"
	: >"$WORK/counts-before.tsv"
	log "step 5: restamp incarnation fences"
	for ctx in "${CONTEXTS[@]}"; do
		c="$(cat "$WORK/$ctx/cluster")"
		list_kind "$ctx" tenants >"$WORK/$ctx/live-tenants.json"
		# Each pair carries the new Tenant uid and the old Team uid (empty when the Team is gone).
		jq -r '.items[] | [.metadata.name, .metadata.uid] | @tsv' "$WORK/$ctx/live-tenants.json" |
			while IFS=$'\t' read -r t u; do
				o="$(kc "$ctx" get teams.varroa.dev "$t" -o jsonpath='{.metadata.uid}' 2>&1)" || { case "$o" in *NotFound* | *"resource type"*) o="" ;; *) die "cannot read the old Team uid for $t on $ctx: $o" ;; esac; }
				printf '%s\t%s\t%s\t%s\n' "$c" "$t" "$u" "$o" >>"$WORK/pairs.tsv"
			done
		if ((DRY)); then
			# Tenants do not exist yet in dry-run. Show the names the cutover would stamp.
			jq -r '.items[] | [.metadata.name, .metadata.uid] | @tsv' "$WORK/$ctx/old-teams.json" |
				while IFS=$'\t' read -r t o; do
					printf '%s\t%s\t%s\t%s\n' "$c" "$t" "<new uid>" "$o" >>"$WORK/pairs.tsv"
				done
		fi
	done
	while IFS=$'\t' read -r c t u o; do
		[[ -n "$c" ]] || continue
		valid_name "$c"
		valid_name "$t"
		if ((DRY)); then
			log "  DRY-RUN: cluster=$c tenant=$t uid=$u old=$o"
		else
			[[ "$u" =~ ^[0-9a-f-]{36}$ ]] || die "unexpected uid for $t: $u"
			if [[ -n "$o" && ! "$o" =~ ^[0-9a-f-]{36}$ ]]; then die "unexpected old uid for $t: $o"; fi
			printf '%s\t%s\t%s\t%s\n' "$c" "$t" \
				"$(db_sql "$SQL_PLACEMENT_COUNT" -v c="$c" -v t="$t")" \
				"$(db_sql "$SQL_RUN_COUNT" -v c="$c" -v t="$t")" >>"$WORK/counts-before.tsv"
		fi
		# A run claimed before the cutover keeps the old uid on its intent, and its
		# completion webhook copies that uid. The full fence restamps those intents.
		if [[ -n "$o" && "$o" != "$u" ]]; then
			db_sql "$SQL_FENCE_FULL" -v c="$c" -v t="$t" -v u="$u" -v o="$o" >/dev/null
		else
			db_sql "$SQL_FENCE_BASE" -v c="$c" -v t="$t" -v u="$u" >/dev/null
		fi
	done <"$WORK/pairs.tsv"

	local pending
	if ((DRY)); then
		log "DRY-RUN: nats consumer info varroa_activity activity-recorder (must report 0 pending)"
	else
		pending="$(kc "$CORE" -n "$NS" exec "deploy/$NATS_BOX" -- nats consumer info varroa_activity activity-recorder --json |
			jq -r '.num_pending')"
		[[ "$pending" == "0" ]] || die "activity-recorder has $pending pending messages; drain before the deploy"
		log "  activity-recorder has 0 pending"
	fi
}

# step 6: wait for the operator Deployment to report the new image.
step6_wait_image() {
	local ctx deadline
	log "step 6: DEPLOY NOW. The operator Deployment must report $IMAGE with one ready replica."
	for ctx in "${CONTEXTS[@]}"; do
		if ((DRY)); then
			log "DRY-RUN: wait for $IMAGE on $ctx"
			continue
		fi
		deadline=$((SECONDS + DEPLOY_TIMEOUT))
		until kc "$ctx" -n "$NS" get deploy "$DEPLOY" -o json |
			jq -e --arg i "$IMAGE" '.status.readyReplicas == 1 and any(.spec.template.spec.containers[]; .image == $i)' >/dev/null; do
			((SECONDS < deadline)) || die "timed out waiting for $IMAGE on $ctx"
			sleep 10
		done
		log "  $ctx runs $IMAGE"
	done
}

# step 7: remove the old objects. The orphan delete keeps the
# children; the owned Group and binding are deleted by name.
step7_retire_old() {
	local ctx="$1" res ns name key ns_args
	log "step 7 [$ctx]: remove Team and TeamAgent, owned Group and binding, old labels, old CRDs"
	for res in teams teamagents; do
		if ! kc "$ctx" get crd "$res.varroa.dev" >/dev/null 2>&1; then continue; fi
		while IFS=/ read -r ns name; do
			[[ -n "$name" ]] || continue
			valid_name "$name"
			ns_args=()
			if [[ -n "$ns" ]]; then ns_args=(-n "$ns"); fi
			act kubectl --context "$ctx" ${ns_args[@]+"${ns_args[@]}"} patch "$res.varroa.dev" "$name" --type merge \
				-p '{"metadata":{"finalizers":null}}'
			act kubectl --context "$ctx" ${ns_args[@]+"${ns_args[@]}"} delete "$res.varroa.dev" "$name" --cascade=orphan --wait=false
		done < <(kc "$ctx" get "$res.varroa.dev" -A -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}')
	done
	for res in groups varroarolebindings; do
		while read -r name; do
			[[ "$name" == team-* ]] || continue
			valid_name "$name"
			act kubectl --context "$ctx" delete "$res.varroa.dev" "$name"
		done < <(kc "$ctx" get "$res.varroa.dev" -l varroa.dev/managed-by=team -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')
	done
	for key in "${OLD_LABEL_KEYS[@]}"; do
		if [[ -n "$(kc "$ctx" get namespaces -l "$key" -o name)" ]]; then
			act kubectl --context "$ctx" label namespaces -l "$key" "$key-"
		fi
		if [[ -n "$(kc "$ctx" get services,serviceaccounts,pods -A -l "$key" -o name)" ]]; then
			act kubectl --context "$ctx" label services,serviceaccounts,pods -A -l "$key" "$key-"
		fi
	done
	for res in teams teamagents; do
		if kc "$ctx" get crd "$res.varroa.dev" >/dev/null 2>&1; then
			act kubectl --context "$ctx" delete crd "$res.varroa.dev"
		fi
	done
}

# step 8: rewrite each Forgejo hibernation webhook that names /teams/.
# The token in the URL stays. The API call uses the token file. tea has
# no call that lists the hooks of every repository.
step8_forgejo() {
	if [[ -z "$FORGE_URL" || -z "$FORGE_TOKEN_FILE" ]]; then
		log "step 8: skipped (set VARROA_FORGE_URL and --forge-token-file)"
		return 0
	fi
	local token page names full hooks hook id newcfg
	token="$(<"$FORGE_TOKEN_FILE")"
	log "step 8: rewrite Forgejo hibernation webhooks"
	page=1
	while :; do
		names="$(curl -fsS -H "Authorization: token $token" \
			"$FORGE_URL/api/v1/repos/search?limit=50&page=$page" | jq -r '.data[].full_name')"
		[[ -n "$names" ]] || break
		while IFS= read -r full; do
			[[ -n "$full" ]] || continue
			hooks="$(curl -fsS -H "Authorization: token $token" "$FORGE_URL/api/v1/repos/$full/hooks")"
			jq -c '.[] | select((.config.url // "") | test("/hibernation/[^/]+/teams/"))' <<<"$hooks" |
				while IFS= read -r hook; do
					id="$(jq -r .id <<<"$hook")"
					newcfg="$(jq -c '.config | .url |= sub("/teams/"; "/tenants/")' <<<"$hook")"
					if ((DRY)); then
						log "DRY-RUN: rewrite hook $id on $full"
					else
						curl -fsS -X PATCH -H "Authorization: token $token" -H 'Content-Type: application/json' \
							-d "$(jq -cn --argjson c "$newcfg" '{config: $c}')" \
							"$FORGE_URL/api/v1/repos/$full/hooks/$id" >/dev/null
					fi
				done
		done <<<"$names"
		page=$((page + 1))
	done
}

# step 9: verify the end state. A dry run has no end state, so main skips it.
step9_verify() {
	local ctx dir leftovers n_bad cell_bad line c t p r after
	for ctx in "${CONTEXTS[@]}"; do
		dir="$WORK/$ctx"
		log "step 9 [$ctx]: verify"
		if kc "$ctx" get crd teams.varroa.dev >/dev/null 2>&1; then fail "$ctx: CRD teams.varroa.dev remains"; fi
		if kc "$ctx" get crd teamagents.varroa.dev >/dev/null 2>&1; then fail "$ctx: CRD teamagents.varroa.dev remains"; fi
		leftovers="$(kc "$ctx" get groups.varroa.dev,varroarolebindings.varroa.dev -o name | grep -E '/team-' || true)"
		if [[ -n "$leftovers" ]]; then fail "$ctx: team-* Group or binding remains: $leftovers"; fi
		n_bad="$(kc "$ctx" get namespaces,services,serviceaccounts,pods -A -o json |
			jq '[.items[] | (.metadata.labels // {}) | keys[] | select(startswith("varroa.dev/team-"))] | length')"
		if ((n_bad != 0)); then fail "$ctx: $n_bad old varroa.dev/team-* label keys remain"; fi
		cell_bad="$(list_kind "$ctx" tenants | jq -r '.items[] | select(.spec.pipelines != null)
			| select(([.status.conditions[]? | select(.reason == "TenantPlaced")] | length) == 0) | .metadata.name')"
		if [[ -n "$cell_bad" ]]; then fail "$ctx: pipelines Tenants not TenantPlaced: $cell_bad"; fi
		if [[ -f "$dir/statuses.jsonl" ]]; then
			while IFS= read -r line; do
				t="$(jq -r .name <<<"$line")"
				p="$(jq -r '.status.placement.cell // ""' <<<"$line")"
				[[ -n "$p" ]] || continue
				r="$(kc "$ctx" get tenants.varroa.dev "$t" -o jsonpath='{.status.placement.cell}' 2>/dev/null || true)"
				if [[ "$r" != "$p" ]]; then fail "$ctx: tenant $t moved from cell $p to ${r:-none}"; fi
			done <"$dir/statuses.jsonl"
		fi
		# The accepted flag of a TenantAgent comes from the BFF, not the CRD.
		# Live check 10.2 covers it. Here a False condition fails the run.
		kc "$ctx" get tenantagents.varroa.dev -A -o json |
			jq -r '.items[] | select([.status.conditions[]? | select(.status == "False")] | length > 0) | .metadata.name' |
			while IFS= read -r t; do fail "$ctx: TenantAgent $t has a False condition"; done
	done
	while IFS=$'\t' read -r c t p r; do
		[[ -n "$c" ]] || continue
		after="$(db_sql "$SQL_PLACEMENT_COUNT" -v c="$c" -v t="$t")"
		if [[ "$after" != "$p" ]]; then fail "placements for $c/$t: before $p, after $after"; fi
		after="$(db_sql "$SQL_RUN_COUNT" -v c="$c" -v t="$t")"
		if [[ "$after" != "$r" ]]; then fail "runs for $c/$t: before $r, after $after"; fi
	done <"$WORK/counts-before.tsv"
	log "step 9: schedules and branch inventory counts are checked by the operator on each announce"
}

main() {
	WORK="$(mktemp -d "${TMPDIR:-/tmp}/migrate-team-to-tenant.XXXXXX")"
	if ((DRY)) && [[ -z "$IMAGE" ]]; then IMAGE="<image ref>"; fi
	write_filters
	log "work directory: $WORK"
	local ctx
	for ctx in "${CONTEXTS[@]}"; do step1_crds "$ctx"; done
	for ctx in "${CONTEXTS[@]}"; do step2_objects "$ctx"; done
	for ctx in "${CONTEXTS[@]}"; do step3_rbac "$ctx"; done
	for ctx in "${CONTEXTS[@]}"; do step4_stop_operator "$ctx"; done
	step5_fences
	step6_wait_image
	for ctx in "${CONTEXTS[@]}"; do step7_retire_old "$ctx"; done
	step8_forgejo
	if ((DRY)); then
		log "dry run: nothing written. Step 9 runs after the cutover."
		exit 0
	fi
	step9_verify
	if ((FAILS > 0)); then
		die "verification failed with $FAILS problem(s)"
	fi
	log "cutover verified"
}

main "$@"
