#!/bin/bash
# Measures whether server-sent events cross CloudPanel's nginx without being held back.
#
# Publishes 3 public Mercure updates, 1 s apart, while an HTTP/1.1 subscriber listens through the CloudPanel vhost
# (HTTP/2 hides the problem: always test HTTP/1.1). Prints how long after publishing each event reached the subscriber;
# "NOT received" means the outer nginx held it back.
#
# Run it ON THE SERVER, as the deploy user, for a stack whose vhost has no custom directive:
#   ssh deploy@server 'bash -s -- <domain> <proxy-port> <deployment-root>' < scripts/sse-check.sh
# Needs python3 and curl; the hub secret is read from <root>/.env and never printed. Topic: dk-sse-check.
# Mercure 1.0 specifics (learned the hard way): the token is an RFC 9068 access token (typ "at+jwt") with iss = the hub's
# trusted issuer, aud = the hub URL as it sees the request, exp, and an RFC 9396 authorization_details grant; the
# subscriber parameter is `match`, not `topic`.
DOM=${1:?usage: sse-check.sh <domain> <proxy-port> <deployment-root>}; PORT=${2:?proxy port}; ROOT=${3:?deployment root}
# A token like the backend's (Mercure protocol 1.0 = RFC 9068 access token + RFC 9396 authorization_details).
# The hub derives its own URL from the request: through the inner proxy that is http://<domain>/.well-known/mercure.
JWT=$(MS=$(sed -n 's/^MERCURE_JWT_SECRET=//p' $ROOT/.env) ISS="https://$DOM/mercure" AUD="http://$DOM/.well-known/mercure" python3 - <<'PY'
import os, hmac, hashlib, base64, json, time, secrets
b = lambda x: base64.urlsafe_b64encode(x).rstrip(b"=")
j = lambda o: json.dumps(o, separators=(",", ":")).encode()
now = int(time.time())
h = b(j({"alg": "HS256", "typ": "at+jwt"}))
p = b(j({"iss": os.environ["ISS"], "aud": os.environ["AUD"], "sub": "app-backend", "client_id": "app-backend",
         "iat": now, "exp": now + 300, "jti": secrets.token_hex(16),
         "authorization_details": [{"type": "https://mercure.rocks/authorization-detail", "actions": ["publish"], "topics": [{"match": "dk-sse-check"}]}]}))
print((h + b"." + p + b"." + b(hmac.new(os.environ["MS"].encode(), h + b"." + p, hashlib.sha256).digest())).decode())
PY
)
arr=$(mktemp); pub=$(mktemp)
( curl -sk -N --http1.1 --max-time 10 --resolve $DOM:443:127.0.0.1 "https://$DOM/.well-known/mercure?match=dk-sse-check" \
   | while IFS= read -r l; do case "$l" in data:*) echo "$(date +%s.%N) $l";; esac; done > "$arr" ) &
sleep 2
for i in 1 2 3; do
  echo "$(date +%s.%N) event-$i" >> "$pub"
  code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:$PORT/.well-known/mercure" -H "Host: $DOM" -H "Authorization: Bearer $JWT" --data-urlencode topic=dk-sse-check --data-urlencode "data=event-$i")
  [ "$code" = 200 ] || echo "   publish event-$i -> HTTP $code (unexpected)"
  sleep 1
done
wait
for i in 1 2 3; do
  ts=$(sed -n "${i}p" "$pub" | cut -d' ' -f1)
  got=$(grep "event-$i\$" "$arr" | head -1 | cut -d' ' -f1)
  if [ -n "$got" ]; then awk -v a="$got" -v p="$ts" -v n="$i" 'BEGIN{printf "   event-%d  received %.1f s after it was published\n", n, a-p}'; else echo "   event-$i  NOT received"; fi
done
rm -f "$arr" "$pub"
