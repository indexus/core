#!/usr/bin/env python3
"""Run the opt-in HTTP handoff fixture with production usage accounting.

The same assertions and fsyncs run as in the test. A normal executable makes
testing.Testing() false, disabling the storage's differential full-data scan.
Only the test runner is replaced; no production code or flags are changed.
"""

import os
from pathlib import Path
import subprocess
import tempfile


root = Path(__file__).resolve().parents[2]
fixture = root / "internal/mesh/handoff_measure_test.go"
source = fixture.read_text()
replacements = {
    "package mesh_test": "package main",
    "func TestMeasuredGranularHandoff(t *testing.T)": "func measure(t *runner)",
}
for before, after in replacements.items():
    if source.count(before) != 1:
        raise SystemExit(f"fixture changed: expected one {before!r}")
    source = source.replace(before, after)

source += r'''

type runner struct { cleanup []func() }
func (r *runner) Cleanup(f func()) { r.cleanup = append(r.cleanup, f) }
func (r *runner) Fatal(v ...any) { panic(fmt.Sprint(v...)) }
func (r *runner) Fatalf(f string, v ...any) { panic(fmt.Sprintf(f, v...)) }
func (r *runner) Skip(v ...any) { panic(fmt.Sprint(v...)) }
func (r *runner) Logf(f string, v ...any) { fmt.Printf(f+"\n", v...) }
func (r *runner) TempDir() string {
    dir, err := os.MkdirTemp("", "indexus-handoff-")
    if err != nil { panic(err) }
    r.Cleanup(func() { os.RemoveAll(dir) })
    return dir
}
func main() {
    r := &runner{}
    defer func() {
        for i := len(r.cleanup)-1; i >= 0; i-- { r.cleanup[i]() }
    }()
    measure(r)
}
'''

env = dict(os.environ, INDEXUS_HANDOFF_MEASURE="1", INDEXUS_MEASURE_PRODUCTION="1")
# Stay within the module, so Go's internal-package import rules still apply.
with tempfile.TemporaryDirectory(prefix="handoff-measure-", dir=root / "tools") as temp:
    directory = Path(temp)
    (directory / "main.go").write_text(source)
    result = subprocess.run(["go", "run", str(directory)], cwd=root, env=env)
    raise SystemExit(result.returncode)
