"""Integration regressions for benchmark projections and relative output paths."""
import json
import subprocess
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]


class BenchmarkIntegration(unittest.TestCase):
    def test_scenario_projection_and_relative_output(self):
        with tempfile.TemporaryDirectory() as tmp:
            exe = Path(tmp) / "vcsimulator"
            subprocess.run(["go", "build", "-o", str(exe), "./cmd/vcsimulator"], cwd=ROOT, check=True)

            def describe(name):
                scenario = ROOT / "examples" / "scenarios" / name
                return json.loads(subprocess.check_output([str(exe), "-scenario", str(scenario), "-describe"]))

            one = describe("phase1.yaml")["projection"]
            large = describe("scale-50.yaml")["projection"]
            # The pinned reference collector captured 240 objects on phase1.
            self.assertEqual(one["all"]["entities"], 240)
            # Uniform 50-cluster profile: independent shares sum to the total.
            shares = list(large["perCluster"].values())
            self.assertEqual(len(shares), 50)
            self.assertEqual(large["all"]["entities"], sum(p["entities"] for p in shares))
            self.assertEqual(large["all"]["series"], sum(p["series"] for p in shares))
            self.assertGreater(shares[0]["entities"], one["all"]["entities"])
            # Run outside the repository using a relative --out, as callers do.
            subprocess.run(["python3", str(ROOT / "tools/bench/bench.py"),
                            str(ROOT / "examples/scenarios/phase1.yaml"), "--runs", "1",
                            "--port", "18571", "--out", "out"], cwd=tmp, check=True,
                           stdout=subprocess.DEVNULL)
            bundle = json.loads((Path(tmp) / "out/benchmark.json").read_text())
            self.assertEqual(bundle["projection"]["totalEntities"], 240)
            self.assertEqual(bundle["projection"]["totalMetricSeries"], one["all"]["series"])
            self.assertEqual(bundle["runs"][0]["unexpectedStatuses"], 0)


if __name__ == "__main__":
    unittest.main()
