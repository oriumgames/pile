#!/bin/sh
# Record or compare performance baselines.
#
#   scripts/bench.sh record            rewrite the checked-in baselines
#   scripts/bench.sh run <dir>         run the same benchmarks into <dir>
#   scripts/bench.sh compare <dir>     benchstat the baselines against <dir>
#
# The parameters are pinned here rather than left to whoever runs it, because a
# benchmark run is only comparable against another run of the same shape. In
# particular -benchtime is time-based and never `Nx`: several benchmarks in this
# tree build a zstd encoder or a dictionary codec on their first iteration, and
# a run of a handful of iterations charges that one-time cost to the whole
# measurement. That mistake has already been made here once, and it read as an
# order-of-magnitude allocation regression. See PERFORMANCE.md.
set -eu

BENCHTIME=${BENCHTIME:-1s}
COUNT=${COUNT:-6}
PKGS='. ./format'

usage() {
	sed -n '2,12p' "$0" >&2
	exit 2
}

run_into() {
	dir=$1
	mkdir -p "$dir"
	for pkg in $PKGS; do
		case $pkg in
		.) out=$dir/benchmarks.txt ;;
		*) out=$dir/$(basename "$pkg")-benchmarks.txt ;;
		esac
		echo "# go test $pkg -run '^\$' -bench=. -benchmem -benchtime=$BENCHTIME -count=$COUNT" >&2
		go test "$pkg" -run '^$' -bench=. -benchmem -benchtime="$BENCHTIME" -count="$COUNT" |
			grep -v '^\(PASS\|ok\|--- \)' >"$out"
	done
}

case ${1:-} in
record)
	run_into testdata
	cp testdata/format-benchmarks.txt format/testdata/benchmarks.txt
	rm testdata/format-benchmarks.txt
	echo "recorded: testdata/benchmarks.txt format/testdata/benchmarks.txt" >&2
	;;
run)
	[ $# -eq 2 ] || usage
	run_into "$2"
	;;
compare)
	[ $# -eq 2 ] || usage
	command -v benchstat >/dev/null 2>&1 || {
		echo "benchstat not found: go install golang.org/x/perf/cmd/benchstat@latest" >&2
		exit 1
	}
	echo "== github.com/oriumgames/pile"
	benchstat testdata/benchmarks.txt "$2/benchmarks.txt"
	echo
	echo "== github.com/oriumgames/pile/format"
	benchstat format/testdata/benchmarks.txt "$2/format-benchmarks.txt"
	;;
*) usage ;;
esac
