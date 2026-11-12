#!/usr/bin/env python3
"""Compare identical ordinary-query workloads against the pre-admission revision."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import random
import re
import shutil
import statistics
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[2]
WORKLOADS = ["internal/system_g/plan/ordinary_benchmark_test.go", "pkg/omnisdk/ordinary_benchmark_test.go"]
PATTERN = re.compile(r"^(BenchmarkOrdinary\S+)\s+\d+\s+([\d.]+) ns/op\s+(\d+) B/op\s+(\d+) allocs/op", re.MULTILINE)


def samples(text):
    out = {}
    for name, ns, size, allocations in PATTERN.findall(text):
        row = out.setdefault(name, {"ns": [], "bytes": [], "allocs": []})
        for key, value in zip(row, [ns, size, allocations]):
            row[key].append(float(value))
    return out


def interval(before, after):
    rng = random.Random(42)
    ratios = sorted(statistics.median(rng.choices(after, k=len(after))) /
                    statistics.median(rng.choices(before, k=len(before))) for _ in range(4000))
    return [ratios[100], ratios[3899]]


def run(command, cwd=ROOT, **kwargs):
    return subprocess.run(command, cwd=cwd, check=True, **kwargs)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--baseline", default="e6b5fa9")
    parser.add_argument("--out", type=Path, default=ROOT / "cicd/out/admission-validation")
    parser.add_argument("--count", type=int, default=8)
    parser.add_argument("--benchtime", default="200ms")
    parser.add_argument("--tolerance", type=float, default=0.10)
    args = parser.parse_args()
    if args.count < 5:
        parser.error("at least five samples are required")
    out = args.out.resolve()
    out.mkdir(parents=True, exist_ok=True)
    baseline = run(["git", "rev-parse", "--verify", args.baseline + "^{commit}"], capture_output=True, text=True).stdout.strip()
    manifest = {"baseline": baseline, "candidate": run(["git", "rev-parse", "HEAD"], capture_output=True, text=True).stdout.strip(),
                "platform": platform.platform(), "machine": platform.machine(), "count": args.count,
                "benchtime": args.benchtime, "cpu": 2, "tolerance": args.tolerance,
                "go": run(["go", "version"], capture_output=True, text=True).stdout.strip(),
                "workloads": {path: hashlib.sha256((ROOT / path).read_bytes()).hexdigest() for path in WORKLOADS}}
    (out / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
    (out / "candidate.patch").write_bytes(run(["git", "diff", "HEAD", "--binary"], capture_output=True).stdout)
    for path in WORKLOADS:
        name = Path(path).name.replace("ordinary_", Path(path).parts[0] + "_ordinary_")
        # These are evidence snapshots, not a Go package. Keep them out of
        # `go test ./...` even when --out is inside the repository.
        shutil.copyfile(ROOT / path, out / (name + ".txt"))
        (out / name).unlink(missing_ok=True)  # Remove artifacts from older runs.
    env = dict(os.environ, GOCACHE=os.environ.get("GOCACHE", "/tmp/omnisdk-go-cache"))
    command = ["go", "test", "./internal/system_g/plan", "./pkg/omnisdk", "-run", "^$",
               "-bench", "^BenchmarkOrdinary", "-benchmem", "-cpu=2", f"-count={args.count}", f"-benchtime={args.benchtime}"]
    with tempfile.TemporaryDirectory(prefix="omni-perf-baseline-", dir="/tmp") as temporary:
        archive = run(["git", "archive", baseline], capture_output=True).stdout
        run(["tar", "-xf", "-", "-C", temporary], input=archive)
        for path in WORKLOADS:
            shutil.copyfile(ROOT / path, Path(temporary) / path)
        shutil.copytree(ROOT / "pkg/omnisdk/testdata/ordinary", Path(temporary) / "pkg/omnisdk/testdata/ordinary", dirs_exist_ok=True)
        # Build/warm both versions before measuring. Runs are sequential to avoid
        # cross-version CPU contention; intervals describe this local experiment.
        for directory in [Path(temporary), ROOT]:
            run(["go", "test", "./internal/system_g/plan", "./pkg/omnisdk", "-run", "^$"], cwd=directory, env=env, capture_output=True)
        for label, directory in [("baseline", Path(temporary)), ("candidate", ROOT)]:
            print(f"Measuring {label}...", flush=True)
            result = run(command, cwd=directory, env=env, capture_output=True, text=True)
            (out / f"{label}.txt").write_text(result.stdout)
    before, after = [samples((out / f"{label}.txt").read_text()) for label in ["baseline", "candidate"]]
    if len(before) != 10 or before.keys() != after.keys():
        raise AssertionError(f"expected ten matching workloads, got {list(before)} and {list(after)}")
    results, passed = {}, True
    lines = ["# Ordinary-query performance comparison", "", f"Baseline: `{baseline}`. Two CPUs; {args.count} samples per workload.", "",
             "Gate: no >10% allocation/byte increase and no latency regression whose 95% bootstrap ratio interval is wholly above +10%.", "",
             "| Workload | Baseline ns/op | Current ns/op | Time delta | 95% ratio interval | B/op before / after | allocs/op before / after | Gate |",
             "| --- | ---: | ---: | ---: | --- | ---: | ---: | --- |"]
    for name in sorted(before):
        old, new = before[name], after[name]
        if len(old["ns"]) != args.count or len(new["ns"]) != args.count:
            raise AssertionError(f"incomplete samples: {name}")
        medians = {label: {key: statistics.median(values) for key, values in data.items()}
                   for label, data in [("baseline", old), ("candidate", new)]}
        a, b = medians.values()
        bounds = interval(old["ns"], new["ns"])
        ok = bounds[0] <= 1 + args.tolerance and all(b[key] <= a[key] * (1 + args.tolerance) for key in ["bytes", "allocs"])
        passed &= ok
        results[name] = {**medians, "latency_ratio_interval": bounds, "passed": ok}
        lines.append(f"| {name} | {a['ns']:.0f} | {b['ns']:.0f} | {(b['ns']/a['ns']-1)*100:+.1f}% | {bounds[0]:.3f}–{bounds[1]:.3f} | {a['bytes']:.0f} / {b['bytes']:.0f} | {a['allocs']:.0f} / {b['allocs']:.0f} | {'PASS' if ok else 'FAIL'} |")
    lines += ["", "These are local microbenchmarks, not proof for every workload or host. Execution uses in-memory read exchanges to expose engine overhead without network noise. SDK planning uses the same tracked document fixtures in both revisions.", ""]
    (out / "summary.json").write_text(json.dumps({"passed": passed, "results": results}, indent=2) + "\n")
    (out / "summary.md").write_text("\n".join(lines))
    print("\n".join(lines), flush=True)
    return 0 if passed else 1


if __name__ == "__main__":
    raise SystemExit(main())
