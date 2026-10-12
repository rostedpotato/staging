package discovery

import (
	"fmt"
	"strings"

	"parkee/staging-platform/internal/config"
)

// remoteScanScript builds a single read-only POSIX shell script that prints
// one JSON array (the same shape remote.go's remoteSlot/remoteService decode)
// to stdout. Everything it reads - `docker ps`, each slot's git checkout,
// nginx sites-enabled - is gathered in one process so the snapshot is
// consistent, and it never writes to the host (no deploy/restart/rm).
func remoteScanScript(cfg config.Discovery) string {
	slots := cfg.Services // not slot names; kept for clarity below
	_ = slots
	services := make([]string, len(cfg.Services))
	copy(services, cfg.Services)

	var b strings.Builder
	b.WriteString("set -u\n")
	fmt.Fprintf(&b, "APP_ROOT=%s\n", shQuote(cfg.AppRoot))
	fmt.Fprintf(&b, "SLOT_PREFIX=%s\n", shQuote(cfg.SlotDirPrefix))
	fmt.Fprintf(&b, "REPO_SUBDIR=%s\n", shQuote(cfg.RepoSubdir))
	fmt.Fprintf(&b, "NGINX_DIR=%s\n", shQuote(cfg.NginxSitesDir))
	fmt.Fprintf(&b, "DOCKER_BIN=%s\n", shQuote(orDefault(cfg.DockerBin, "docker")))
	fmt.Fprintf(&b, "SERVICES=%s\n", shQuote(strings.Join(services, " ")))

	b.WriteString(remoteScanScriptBody)
	return b.String()
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// shQuote wraps a value in single quotes for safe embedding in the generated
// shell script, escaping any embedded single quotes. Config values are
// operator-controlled (not user input), but this keeps the script correct
// even if a path contains spaces or special characters.
func shQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

// remoteScanScriptBody does the actual work. It's deliberately conservative:
// every external command's failure is tolerated (|| true / 2>/dev/null) so
// one missing slot or unreadable file doesn't abort the whole scan, and it
// never calls anything that could mutate state (no docker run/stop/rm, no
// git write commands).
const remoteScanScriptBody = `
json_escape() {
  printf '%s' "$1" | python3 -c 'import json,sys; print(json.dumps(sys.stdin.read()))' 2>/dev/null \
    || printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g' | awk '{printf "\"%s\"", $0}'
}

DOCKER_PS=""
if command -v "$DOCKER_BIN" >/dev/null 2>&1; then
  DOCKER_PS="$($DOCKER_BIN ps -a --format '{{.Names}}|{{.Image}}|{{.State}}|{{.Status}}|{{.Ports}}' 2>/dev/null)"
fi

container_field() {
  # $1=container name  $2=field index (2=image,3=state,4=status,5=ports)
  printf '%s\n' "$DOCKER_PS" | awk -F'|' -v want="$1" -v idx="$2" '$1==want {print $idx; found=1} END{if(!found) exit 1}'
}

health_from_status() {
  case "$1" in
    *'(healthy)'*) echo healthy ;;
    *'(unhealthy)'*) echo unhealthy ;;
    *'(health: starting)'*|*starting*) echo starting ;;
    *) echo none ;;
  esac
}

first_host_port() {
  printf '%s\n' "$1" | tr ',' '\n' | while read -r part; do
    case "$part" in
      *'->'*)
        hostside="${part%%->*}"
        echo "${hostside##*:}" | tr -d ' '
        return
        ;;
    esac
  done
}

slot_names=""
if [ -d "$APP_ROOT" ]; then
  for d in "$APP_ROOT"/"$SLOT_PREFIX"*/; do
    [ -d "$d" ] || continue
    base="$(basename "$d")"
    name="${base#$SLOT_PREFIX}"
    case "$name" in
      *-bak|*_bak*) continue ;;
    esac
    slot_names="$slot_names $name"
  done
fi

printf '['
first_slot=1
for name in $slot_names; do
  dir="$APP_ROOT/$SLOT_PREFIX$name"
  repo_dir="$dir/$REPO_SUBDIR"
  branch=""; commit=""; commit_time=""
  if [ -d "$repo_dir/.git" ] && command -v git >/dev/null 2>&1; then
    branch="$(git -c safe.directory='*' -C "$repo_dir" rev-parse --abbrev-ref HEAD 2>/dev/null)"
    commit="$(git -c safe.directory='*' -C "$repo_dir" rev-parse --short HEAD 2>/dev/null)"
    commit_time="$(git -c safe.directory='*' -C "$repo_dir" log -1 --format=%ci 2>/dev/null)"
  fi

  domains=""
  if [ -d "$NGINX_DIR" ]; then
    domains="$(grep -RhsE '^\s*server_name\s' "$NGINX_DIR" 2>/dev/null \
      | grep -F "$name" \
      | sed -E 's/^\s*server_name\s+(.*);.*/\1/' \
      | tr ' ' '\n' | grep -v '^~' | sort -u | tr '\n' ',' )"
  fi

  if [ "$first_slot" -eq 1 ]; then first_slot=0; else printf ','; fi
  printf '{"name":%s,"dir":%s,"branch":%s,"commit":%s,"commitTime":%s,"domains":[' \
    "$(json_escape "$name")" "$(json_escape "$dir")" "$(json_escape "$branch")" \
    "$(json_escape "$commit")" "$(json_escape "$commit_time")"
  first_dom=1
  OLD_IFS="$IFS"; IFS=','
  for dom in $domains; do
    [ -n "$dom" ] || continue
    if [ "$first_dom" -eq 1 ]; then first_dom=0; else printf ','; fi
    printf '%s' "$(json_escape "$dom")"
  done
  IFS="$OLD_IFS"
  printf '],"services":['

  first_svc=1
  for svc in $SERVICES; do
    want="$svc-$name"
    if [ "$first_svc" -eq 1 ]; then first_svc=0; else printf ','; fi
    if out="$(container_field "$want" 2)"; then
      image="$out"
      state="$(container_field "$want" 3)"
      status="$(container_field "$want" 4)"
      ports="$(container_field "$want" 5)"
      health="$(health_from_status "$status")"
      hostport="$(first_host_port "$ports")"
      printf '{"name":%s,"found":true,"state":%s,"status":%s,"health":%s,"hostPort":%s,"image":%s}' \
        "$(json_escape "$svc")" "$(json_escape "$(echo "$state" | tr A-Z a-z)")" "$(json_escape "$status")" \
        "$(json_escape "$health")" "$(json_escape "$hostport")" "$(json_escape "$image")"
    else
      printf '{"name":%s,"found":false,"state":"missing","status":"","health":"none","hostPort":"","image":""}' \
        "$(json_escape "$svc")"
    fi
  done
  printf ']}'
done
printf ']\n'
`
