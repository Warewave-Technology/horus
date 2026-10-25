#!/usr/bin/env bash
# End-to-end test: horus runs on this host, the operator runs in a kind
# cluster, and annotated Ingresses (with a faked load balancer status, since
# no ingress controller runs) must resolve through horus.
#
#   make e2e                 # or: test/e2e/kind.sh
#   KEEP=1 make e2e          # keep the kind cluster and horus running afterwards
#
# Needs: go, docker, kind, kubectl >= 1.24, dig, openssl, curl, perl.
# Uses its own kubeconfig, so the current kubectl context is never touched.
#
# Environment:
#   CLUSTER          kind cluster name              (horus-e2e)
#   HORUS_E2E_HOST   how pods reach this host       (host.docker.internal on
#                    Docker Desktop/OrbStack, else the kind network gateway)
#   DNS_PORT         horus DNS port on 127.0.0.1    (15300)
#   API_PORT         horus API port on 0.0.0.0      (18300)
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
CLUSTER=${CLUSTER:-horus-e2e}
DNS_PORT=${DNS_PORT:-15300}
API_PORT=${API_PORT:-18300}
ZONE=e2e.horus.test
ZONE2=edge.horus.test
IMAGE=horus-operator:e2e
TIMEOUT=${TIMEOUT:-90}

WORK=$(mktemp -d "${TMPDIR:-/tmp}/horus-e2e.XXXXXX")
export KUBECONFIG=$WORK/kubeconfig
HORUS_PID=

log() { printf '\n==> %s\n' "$*"; }
fail() {
	printf '\nFAIL: %s\n' "$*" >&2
	echo "--- horus log (tail)" >&2
	tail -n 30 "$WORK/horus.log" >&2 || true
	echo "--- operator log (tail)" >&2
	kubectl -n horus-system logs deploy/horus-operator --tail=30 >&2 || true
	exit 1
}

cleanup() {
	local rc=$?
	if [[ ${KEEP:-0} == 1 ]]; then
		echo
		echo "KEEP=1: leaving things running."
		echo "  export KUBECONFIG=$KUBECONFIG"
		echo "  horus pid $HORUS_PID, log $WORK/horus.log"
		echo "  dig @127.0.0.1 -p $DNS_PORT web.$ZONE"
		echo "  cleanup: kill $HORUS_PID; kind delete cluster --name $CLUSTER; rm -rf $WORK"
		return
	fi
	[[ -n $HORUS_PID ]] && kill "$HORUS_PID" 2>/dev/null && wait "$HORUS_PID" 2>/dev/null
	kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
	rm -rf "$WORK"
	exit $rc
}
trap cleanup EXIT

for bin in go docker kind kubectl dig openssl curl perl; do
	command -v "$bin" >/dev/null || fail "$bin not found"
done

# resolve NAME QTYPE: prints the short answer from horus.
resolve() { dig @127.0.0.1 -p "$DNS_PORT" +short +tries=1 +time=2 "$1" "$2"; }

# wait_for DESCRIPTION COMMAND...: retries COMMAND until it succeeds.
wait_for() {
	local what=$1
	shift
	for ((i = 0; i < TIMEOUT; i++)); do
		if "$@" >/dev/null 2>&1; then
			echo "ok: $what"
			return
		fi
		sleep 1
	done
	fail "timed out waiting for: $what"
}

answer_is() { [[ $(resolve "$1" "$2" | sort | tr '\n' ' ') == "$3 " ]]; }
# gone NAME: horus has no A record for NAME. Names horus does not hold are
# forwarded upstream (NXDOMAIN there, or SERVFAIL without internet), so check
# for an empty answer rather than a specific rcode.
gone() { [[ -z $(resolve "$1" A) ]]; }
has_event() { kubectl get events -n default --field-selector "reason=$1" -o name | grep -q .; }

log "build"
make -C "$ROOT" build >/dev/null
make -C "$ROOT" operator-image VERSION=e2e >/dev/null 2>&1 || fail "operator image build failed"
HORUS=$ROOT/bin/horus

log "kind cluster $CLUSTER"
if ! kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
	kind create cluster --name "$CLUSTER" --kubeconfig "$KUBECONFIG" --wait 120s
else
	kind export kubeconfig --name "$CLUSTER" --kubeconfig "$KUBECONFIG"
fi
kind load docker-image "$IMAGE" --name "$CLUSTER"

if [[ -z ${HORUS_E2E_HOST:-} ]]; then
	if [[ $(uname -s) == Darwin ]]; then
		HORUS_E2E_HOST=host.docker.internal # Docker Desktop, OrbStack, colima
	else
		HORUS_E2E_HOST=$(docker network inspect kind \
			-f '{{range .IPAM.Config}}{{if .Gateway}}{{.Gateway}} {{end}}{{end}}' | tr ' ' '\n' | grep -m1 '\.')
	fi
fi
echo "pods reach horus at $HORUS_E2E_HOST:$API_PORT"

log "start horus (TLS API on :$API_PORT, DNS on 127.0.0.1:$DNS_PORT)"
mkdir -p "$WORK/creds"
"$HORUS" token >"$WORK/creds/admin-token"
SAN="DNS:localhost,IP:127.0.0.1"
if [[ $HORUS_E2E_HOST =~ ^[0-9.]+$ ]]; then SAN+=",IP:$HORUS_E2E_HOST"; else SAN+=",DNS:$HORUS_E2E_HOST"; fi
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 -subj /CN=horus-e2e \
	-addext "subjectAltName=$SAN" -keyout "$WORK/creds/api-tls-key" -out "$WORK/creds/api-tls-cert" 2>/dev/null
CREDENTIALS_DIRECTORY=$WORK/creds \
	HORUS_DNS_LISTEN=127.0.0.1:$DNS_PORT \
	HORUS_API_LISTEN=:$API_PORT \
	HORUS_API_SOCKET=$WORK/api.sock \
	HORUS_DB=$WORK/horus.db \
	HORUS_BACKUP_DIR=$WORK/backups \
	"$HORUS" serve >"$WORK/horus.log" 2>&1 &
HORUS_PID=$!
wait_for "horus API" curl -sf --cacert "$WORK/creds/api-tls-cert" "https://127.0.0.1:$API_PORT/healthz"

cli() { HORUS_ADDR=unix://$WORK/api.sock HORUS_TOKEN_FILE=$WORK/creds/admin-token "$HORUS" "$@"; }
cli zone add "$ZONE" --ttl 60
cli zone add "$ZONE2" --ttl 60
cli record add "taken.$ZONE" A 10.99.9.9
TOKEN=$(cli --json cluster add kind --zone "$ZONE" | sed -n 's/.*"token": "\(.*\)".*/\1/p')
[[ -n $TOKEN ]] || fail "no cluster token"

log "deploy operator"
kubectl create namespace horus-system --dry-run=client -o yaml | kubectl apply -f -
kubectl -n horus-system create secret generic horus-operator-token \
	--from-literal=token="$TOKEN" --from-file=ca.crt="$WORK/creds/api-tls-cert" \
	--dry-run=client -o yaml | kubectl apply -f -
IMAGE=$IMAGE URL=https://$HORUS_E2E_HOST:$API_PORT perl -pe '
	s#^(\s+image: )\S+#$1$ENV{IMAGE}#;
	s#https://horus.internal.example:8053#$ENV{URL}#;
	s#^(\s+)- --leader-elect$#$1- --leader-elect\n$1- --ca-file=/var/run/secrets/horus/ca.crt\n$1- --gc-interval=5s\n$1- --zone-refresh=5s#;
' "$ROOT/operator/config/operator.yaml" | kubectl apply -f -
kubectl -n horus-system rollout status deploy/horus-operator --timeout=120s || fail "operator did not become ready"

lb_ip() { # INGRESS IP-or-hostname
	local field=ip
	[[ $2 =~ ^[0-9.:a-f]+$ ]] || field=hostname
	kubectl patch ingress "$1" --subresource=status --type=merge \
		-p "{\"status\":{\"loadBalancer\":{\"ingress\":[{\"$field\":\"$2\"}]}}}" >/dev/null
}

ingress() { # NAME HOST [extra annotation lines]
	kubectl apply -f - <<EOF
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: $1
  namespace: default
  annotations:
    horus.warewave.tech/register: "true"
${3:-}
spec:
  rules:
    - host: $2
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: web
                port:
                  number: 80
EOF
}

log "1. Ingress with an LB IP resolves to an A record"
ingress web "web.$ZONE"
lb_ip web 10.99.0.10
wait_for "web.$ZONE A 10.99.0.10" answer_is "web.$ZONE" A 10.99.0.10
wait_for "Registered event" has_event Registered

log "2. LB address change updates the record"
lb_ip web 10.99.0.11
wait_for "web.$ZONE A 10.99.0.11" answer_is "web.$ZONE" A 10.99.0.11

log "3. LB hostname becomes a CNAME, TTL annotation applies"
ingress lb "lb.$ZONE" '    horus.warewave.tech/ttl: "42"'
lb_ip lb lb.cloud.example.net
wait_for "lb.$ZONE CNAME" answer_is "lb.$ZONE" CNAME lb.cloud.example.net.
ttl=$(dig @127.0.0.1 -p "$DNS_PORT" +noall +answer "lb.$ZONE" CNAME | awk '{print $2}')
[[ $ttl == 42 ]] || fail "TTL annotation: got $ttl, want 42"

log "4. a name held by a manual record is a conflict; the rest still registers"
ingress mixed "taken.$ZONE" '    horus.warewave.tech/hostnames: "taken.'"$ZONE"',free.'"$ZONE"'"'
lb_ip mixed 10.99.0.20
wait_for "free.$ZONE A 10.99.0.20" answer_is "free.$ZONE" A 10.99.0.20
wait_for "HostnameConflict event" has_event HostnameConflict
answer_is "taken.$ZONE" A 10.99.9.9 || fail "manual record was overwritten"

log "5. removing the annotation removes the records"
kubectl annotate ingress mixed horus.warewave.tech/register- >/dev/null
wait_for "free.$ZONE gone" gone "free.$ZONE"

log "6. deleting the Ingress removes the records"
kubectl delete ingress lb >/dev/null
wait_for "lb.$ZONE gone" gone "lb.$ZONE"

log "7. garbage collection catches deletes made while the operator was down"
kubectl -n horus-system scale deploy/horus-operator --replicas=0 >/dev/null
kubectl -n horus-system wait --for=delete pod -l app.kubernetes.io/name=horus-operator --timeout=60s >/dev/null
kubectl delete ingress web >/dev/null
answer_is "web.$ZONE" A 10.99.0.11 || fail "web.$ZONE should still resolve while the operator is down"
kubectl -n horus-system scale deploy/horus-operator --replicas=1 >/dev/null
wait_for "web.$ZONE gone after restart" gone "web.$ZONE"

log "8. a second zone added to the cluster at runtime is picked up without a restart"
ingress multi "a.$ZONE" '    horus.warewave.tech/hostnames: "a.'"$ZONE"',a.'"$ZONE2"'"'
lb_ip multi 10.99.0.30
wait_for "a.$ZONE A 10.99.0.30" answer_is "a.$ZONE" A 10.99.0.30
wait_for "HostSkipped event for a.$ZONE2" has_event HostSkipped
gone "a.$ZONE2" || fail "a.$ZONE2 must not resolve before $ZONE2 is assigned"
cli cluster zone add kind "$ZONE2"
wait_for "a.$ZONE2 A 10.99.0.30" answer_is "a.$ZONE2" A 10.99.0.30
cli --json whoami >/dev/null

log "9. removing that zone deletes the cluster's records in it, the rest stays"
cli cluster zone rm kind "$ZONE2"
wait_for "a.$ZONE2 gone" gone "a.$ZONE2"
answer_is "a.$ZONE" A 10.99.0.30 || fail "a.$ZONE must survive the removal of $ZONE2"
sleep 7 # one zone refresh: the operator must not re-add the name
gone "a.$ZONE2" || fail "a.$ZONE2 came back after the zone was removed"

log "10. names horus does not hold are forwarded, also inside a zone of ours"
cli owners
dig @127.0.0.1 -p "$DNS_PORT" +tries=1 +time=3 example.com A | grep -q 'status: NOERROR' ||
	echo "warning: forwarding check failed (no internet access?)"
cli zone add example.com >/dev/null
cli record add "internal.example.com" A 10.99.0.40 >/dev/null
answer_is internal.example.com A 10.99.0.40 || fail "internal.example.com must come from horus"
if dig @127.0.0.1 -p "$DNS_PORT" +tries=1 +time=3 www.example.com A | grep -q 'status: NOERROR'; then
	[[ -n $(resolve www.example.com A) ]] || fail "www.example.com (public, inside our zone example.com) must be forwarded"
	echo "ok: public name inside our zone forwarded"
else
	echo "warning: skipped the public-name check (no internet access?)"
fi

printf '\nPASS: e2e\n'
