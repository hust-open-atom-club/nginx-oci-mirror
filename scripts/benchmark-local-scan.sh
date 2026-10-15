#!/bin/sh
# Run from the checkout to compare the original scanner with the same fixtures.
# Set TMPDIR to a disk-backed directory; repeated scans still warm the page cache.
set -eu
cd "$(git rev-parse --show-toplevel)"
base=${1:-3b3c9838bb5ebd80f5086f0e4d97bfb07a77867c}
fixture="$PWD/inventory_bench_test.go"
temporary=$(mktemp -d)
baseline="$temporary/baseline"
cleanup() {
    git worktree remove --force "$baseline" >/dev/null 2>&1 || true
    rm -rf "$temporary"
}
trap cleanup EXIT HUP INT TERM
git worktree add --detach "$baseline" "$base"
python3 - "$fixture" "$baseline" <<'PY'
import pathlib
import sys

fixture, baseline = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2])
source = fixture.read_text()
source = source.replace('[]int{1, 2, 4}', '[]int{1}')
source = source.replace('scan := &localScan{}', 'benchmarkBlobBytes = 0; benchmarkBlobChecks = 0')
source = source.replace('scan.manifests(b.Context(), fixture.dir, workers, io.Discard)', 'localManifests(fixture.dir)')
source = source.replace('scan.bytes.Load()', 'benchmarkBlobBytes').replace('scan.checked.Load()', 'benchmarkBlobChecks')
source = source.replace('\n\t"io"', '')
(baseline / 'inventory_bench_test.go').write_text(source)

# Original verification is serial: count actual reads and checks, without
# changing its traversal, hash algorithm, or failure handling.
path = baseline / 'files.go'
source = path.read_text()
original = 'return copyContent(io.Discard, f, desc) == nil'
assert source.count(original) == 1, 'baseline validContent changed; review the adapter'
source = source.replace(original, 'benchmarkBlobChecks++; return copyContent(io.Discard, benchmarkCountingReader{f}, desc) == nil')
source += '''
var benchmarkBlobBytes int64
var benchmarkBlobChecks int64
type benchmarkCountingReader struct { io.Reader }
func (r benchmarkCountingReader) Read(p []byte) (int, error) {
    n, err := r.Reader.Read(p)
    benchmarkBlobBytes += int64(n)
    return n, err
}
'''
path.write_text(source)
PY
gofmt -w "$baseline/inventory_bench_test.go" "$baseline/files.go"
printf '\nBaseline %s (serial):\n' "$base"
(cd "$baseline" && go test -run '^$' -bench BenchmarkLocalInventory -benchtime=5x -count=3 ./...)
printf '\nCurrent checkout (1/2/4 verification workers):\n'
go test -run '^$' -bench BenchmarkLocalInventory -benchtime=5x -count=3 ./...
