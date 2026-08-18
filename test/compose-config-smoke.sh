#!/bin/bash
# Black-box smoke of the compose front-end against the real binary.
#
# Usage: test/compose-config-smoke.sh <path-to-hull-binary>
#
# CI runs this in the build job right after `make macos`; run it locally the
# same way against a fresh build. It asserts the contract of `compose config`:
# the supported surface renders deterministically, ignored keys warn loudly on
# stderr, and statically detectable mistakes fail at load.
#
# The document on stdout is compose-go's canonical form, the
# same shape `docker compose config` prints: environment is a mapping rather
# than a KEY=value list, byte quantities render as byte counts rather than
# "<N>m", and keys hull ignores still appear (stderr names them).
# Values that the VM backend cannot honor as declared are the exception and
# render as what it actually gets: cpus rounded up to whole vCPUs, mem_limit
# floored to whole megabytes. The exhaustive per-capability checks live in the
# integration suite; this script proves the shipped binary honors the same
# contract end to end.
set -euo pipefail

BIN=$1
[ -x "$BIN" ] || { echo "not executable: $BIN" >&2; exit 1; }
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

cfg() { "$BIN" compose --file "$1" --project-name smoke config; }

# --- supported surface renders deterministically -----------------------------
cat > "$WORK/base.yaml" <<'EOF'
services:
  web:
    image: alpine:3.19
    cpus: 0.5
    mem_limit: 1g
    environment:
      FOO: bar
      EMPTY:
    labels:
      app: smoke
EOF
out=$(cfg "$WORK/base.yaml" 2>"$WORK/base.err") || fail "base file must load"
printf '%s\n' "$out" | grep -q 'image: alpine:3.19'  || fail "image must render"
printf '%s\n' "$out" | grep -Eq 'cpus: 1$'           || fail "0.5 cpus must round up to 1 vCPU"
printf '%s\n' "$out" | grep -Eq 'mem_limit: "1073741824"$' || fail "1g must render as its byte count"
printf '%s\n' "$out" | grep -Eq '^ +EMPTY: null$'    || fail "bare env key must render unresolved"
# Ignored keys now render. The document is compose-go's canonical form, so it
# shows the file as merged and interpolated; stderr is what says which keys
# hull does not honor. Filtering them back out would mean rebuilding
# the hand-maintained key whitelist that was deleted.
printf '%s\n' "$out" | grep -q 'labels'              || fail "canonical output must render the declared document"
grep -q 'ignoring unsupported key "services.web.labels"' "$WORK/base.err" \
  || fail "ignored key must warn on stderr"

out2=$(cfg "$WORK/base.yaml" 2>/dev/null)
[ "$out" = "$out2" ] || fail "rendering must be deterministic"

# --- statically detectable mistakes fail at load -----------------------------
cat > "$WORK/badhv.yaml" <<'EOF'
services:
  web:
    image: alpine:3.19
    x-hypervisor: bogus
EOF
if cfg "$WORK/badhv.yaml" >/dev/null 2>&1; then
  fail "unknown x-hypervisor must fail at load"
fi

# --- interpolation and env_file ----------------------------------------------
mkdir -p "$WORK/interp"
cat > "$WORK/interp/compose.yaml" <<'EOF'
services:
  web:
    image: alpine:${SMOKE_TAG:-3.19}
    environment:
      PROBE: ${SMOKE_PROBE}
    env_file: smoke.env
EOF
printf 'SMOKE_PROBE=fromdotenv\n' > "$WORK/interp/.env"
printf 'EXTRA=fromenvfile\n' > "$WORK/interp/smoke.env"

out=$(cfg "$WORK/interp/compose.yaml" 2>/dev/null) || fail "interpolation file must load"
printf '%s\n' "$out" | grep -q 'image: alpine:3.19'  || fail "unset var must take its default"
printf '%s\n' "$out" | grep -Eq '^ +PROBE: fromdotenv$' || fail "sibling .env must feed interpolation"
printf '%s\n' "$out" | grep -Eq '^ +EXTRA: fromenvfile$' || fail "env_file values must reach environment"

out=$(SMOKE_TAG=edge SMOKE_PROBE=fromproc cfg "$WORK/interp/compose.yaml" 2>/dev/null) \
  || fail "interpolation file must load with process env"
printf '%s\n' "$out" | grep -q 'image: alpine:edge'   || fail "process env must be used"
printf '%s\n' "$out" | grep -Eq '^ +PROBE: fromproc$'  || fail "process env must beat .env"

cat > "$WORK/interp/required.yaml" <<'EOF'
services:
  web:
    image: alpine:${SMOKE_MISSING:?is required}
EOF
if cfg "$WORK/interp/required.yaml" >/dev/null 2>&1; then
  fail "a :? required marker on an unset variable must fail at load"
fi

cat > "$WORK/interp/missingenvfile.yaml" <<'EOF'
services:
  web:
    image: alpine:3.19
    env_file: does-not-exist.env
EOF
if cfg "$WORK/interp/missingenvfile.yaml" >/dev/null 2>&1; then
  fail "a missing required env_file must fail at load"
fi

# --- named volumes -----------------------------------------------------------
mkdir -p "$WORK/vol" "$WORK/store"
cat > "$WORK/vol/declared.yaml" <<'EOF'
volumes:
  vdata: {}
services:
  web:
    image: alpine:3.19
    volumes:
      - vdata:/data
EOF
out=$("$BIN" --store-dir "$WORK/store" compose --file "$WORK/vol/declared.yaml" \
  --project-name smoke config 2>/dev/null) || fail "declared named volume must load"
printf '%s\n' "$out" | grep -q "$WORK/store/volumes/smoke_vdata:/data" \
  || fail "a declared volume must resolve to <store>/volumes/<project>_<name>"

cat > "$WORK/vol/undeclared.yaml" <<'EOF'
services:
  web:
    image: alpine:3.19
    volumes:
      - vdata:/data
EOF
if cfg "$WORK/vol/undeclared.yaml" >/dev/null 2>&1; then
  fail "an undeclared named volume must fail at load"
fi

cat > "$WORK/vol/hostile.yaml" <<'EOF'
volumes:
  '../../escape': {}
services:
  web:
    image: alpine:3.19
EOF
if cfg "$WORK/vol/hostile.yaml" >/dev/null 2>&1; then
  fail "a volume name that would escape the volumes root must be rejected"
fi

cat > "$WORK/vol/opts.yaml" <<'EOF'
volumes:
  vdata:
    driver: local
services:
  web:
    image: alpine:3.19
    volumes:
      - vdata:/data
EOF
cfg "$WORK/vol/opts.yaml" >/dev/null 2>"$WORK/vol/opts.err" \
  || fail "declaration options must warn, not fail"
grep -q 'ignoring unsupported key "volumes.vdata.driver"' "$WORK/vol/opts.err" \
  || fail "declaration options must warn individually"

# --- one-shot services and the completion gate -------------------------------
mkdir -p "$WORK/oneshot"
cat > "$WORK/oneshot/ok.yaml" <<'EOF'
services:
  mig:
    image: alpine:3.19
    x-oneshot: true
    command: /bin/migrate
  web:
    image: alpine:3.19
    depends_on:
      mig:
        condition: service_completed_successfully
EOF
out=$(cfg "$WORK/oneshot/ok.yaml" 2>/dev/null) || fail "a one-shot job with a command must load"
printf '%s\n' "$out" | grep -q 'x-oneshot: true' \
  || fail "x-oneshot must render in the effective config"
printf '%s\n' "$out" | grep -q 'condition: service_completed_successfully' \
  || fail "the completion condition must render in the effective config"

cat > "$WORK/oneshot/nocmd.yaml" <<'EOF'
services:
  mig:
    image: alpine:3.19
    x-oneshot: true
EOF
if cfg "$WORK/oneshot/nocmd.yaml" >/dev/null 2>&1; then
  fail "x-oneshot without a command must fail at load, not after the VM boots"
fi

# --- exec layer: static surface ----------------------------------------------
# -h prints the exec usage line instead of an unknown-flag error (the hand
# parser owns help since SkipFlagParsing removed the generated one).
if "$BIN" compose exec -h >"$WORK/exec-h.out" 2>&1; then
  fail "compose exec -h exits non-zero by contract (usage, no service)"
fi
grep -q 'usage: compose exec' "$WORK/exec-h.out" \
  || fail "compose exec -h must print usage"
grep -qi 'unknown' "$WORK/exec-h.out" \
  && fail "compose exec -h must not read as an unknown flag"

cat > "$WORK/healthcheck.yaml" <<'EOF'
services:
  web:
    image: alpine:3.19
    healthcheck:
      test: ["CMD", "true"]
    x-healthcheck-tcp:
      port: 80
EOF
out=$(cfg "$WORK/healthcheck.yaml" 2>"$WORK/hc.err") || fail "healthcheck file must load"
printf '%s\n' "$out" | grep -q 'healthcheck:' \
  || fail "the exec-form healthcheck must render in the effective config"
grep -q 'the exec healthcheck wins' "$WORK/hc.err" \
  || fail "declaring both healthcheck forms must warn that the exec form wins"

cat > "$WORK/badhook.yaml" <<'EOF'
services:
  web:
    image: alpine:3.19
    post_start:
      - {}
EOF
if cfg "$WORK/badhook.yaml" >/dev/null 2>&1; then
  fail "a post_start hook without a command must fail at load"
fi

echo "compose-config smoke: OK"
