#!/usr/bin/env bash
# Runs every test in the repository and fails if any test is skipped.
#
# A plain `go test ./...` skips the tests that need Postgres, and on a Mac
# whose ffmpeg has no drawtext filter it skips the two render tests as well.
# This script starts a throwaway Postgres for the first kind and runs the
# second kind inside a container that has the image's ffmpeg.
#
# Needs: go, docker. Usage: scripts/test-all.sh
set -euo pipefail

cd "$(dirname "$0")/.."

postgres_image="postgres:17-alpine"
ffmpeg_image="dwellings-test-ffmpeg"
ffmpeg_packages="./internal/bumper ./internal/video"
container="dwellings-test-pg-$$"
work_dir="$(mktemp -d)"
host_log="$work_dir/host.log"
ffmpeg_log="$work_dir/ffmpeg.log"
rescued=0

cleanup() {
	docker rm -f "$container" >/dev/null 2>&1 || true
	rm -rf "$work_dir"
}
trap cleanup EXIT

echo "==> starting Postgres ($postgres_image)"
docker run -d --rm --name "$container" \
	-e POSTGRES_USER=dwellings -e POSTGRES_PASSWORD=dwellings -e POSTGRES_DB=dwellings \
	-p 127.0.0.1::5432 "$postgres_image" >/dev/null
port="$(docker port "$container" 5432/tcp | head -1 | sed 's/.*://')"
# The server restarts once while the image initialises, so ask for the
# database itself and not only for an open socket.
ready=""
for _ in $(seq 1 60); do
	if docker exec "$container" psql -U dwellings -d dwellings -qtc 'SELECT 1' >/dev/null 2>&1; then
		ready="yes"
		break
	fi
	sleep 1
done
if [ -z "$ready" ]; then
	echo "Postgres did not become ready" >&2
	exit 1
fi
export TEST_DATABASE_URL="postgres://dwellings:dwellings@127.0.0.1:$port/dwellings?sslmode=disable"

echo "==> go vet"
go vet ./...

echo "==> go test -race ./... (with Postgres)"
if ! go test -race -count=1 -v ./... >"$host_log" 2>&1; then
	grep -E '^\s*--- FAIL|^(FAIL|panic:)' "$host_log" || true
	echo "tests failed; full output: $host_log" >&2
	trap 'docker rm -f "$container" >/dev/null 2>&1 || true' EXIT
	exit 1
fi
grep -E '^(ok|FAIL|\?)' "$host_log"

skipped="$(grep -E '^\s*--- SKIP' "$host_log" | sed -E 's/^\s*--- SKIP: ([^ ]+).*/\1/' | sort -u || true)"

if [ -n "$skipped" ] && ! ffmpeg -hide_banner -filters 2>/dev/null | grep -q ' drawtext '; then
	echo "==> local ffmpeg has no drawtext filter: running ${ffmpeg_packages} in a container"
	docker build -q -t "$ffmpeg_image" - >/dev/null <<-'DOCKERFILE'
		FROM alpine:3.20
		RUN apk add --no-cache ffmpeg font-dejavu
	DOCKERFILE
	arch="$(docker version --format '{{.Server.Arch}}')"
	mkdir "$work_dir/bin"
	for package in $ffmpeg_packages; do
		name="$(basename "$package")"
		CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go test -c -o "$work_dir/bin/$name.test" "$package"
		docker run --rm --network none \
			-v "$work_dir/bin:/tests:ro" -v "$PWD:/src:ro" -w "/src/${package#./}" \
			--entrypoint "/tests/$name.test" "$ffmpeg_image" -test.v -test.count=1 >>"$ffmpeg_log" 2>&1 || {
			grep -E '^\s*--- FAIL|^(FAIL|panic:)' "$ffmpeg_log" || true
			echo "tests failed in the ffmpeg container; full output: $ffmpeg_log" >&2
			trap 'docker rm -f "$container" >/dev/null 2>&1 || true' EXIT
			exit 1
		}
	done
	still_skipped=""
	for test_name in $skipped; do
		if grep -qE "^\s*--- PASS: $test_name " "$ffmpeg_log"; then
			echo "ok  	$test_name (in the ffmpeg container)"
			rescued=$((rescued + 1))
		else
			still_skipped="$still_skipped $test_name"
		fi
	done
	skipped="$still_skipped"
fi

if [ -n "${skipped// /}" ]; then
	echo "these tests were skipped, which counts as a failure:" >&2
	for test_name in $skipped; do
		grep -hE -B1 "^\s*--- SKIP: $test_name " "$host_log" "$ffmpeg_log" 2>/dev/null | grep -v '^--$' >&2 || true
	done
	exit 1
fi

passed="$(grep -cE '^--- PASS' "$host_log" || true)"
echo "==> all tests ran: $passed passed locally$([ "$rescued" -gt 0 ] && echo ", $rescued more in the ffmpeg container"), 0 skipped, 0 failed"
