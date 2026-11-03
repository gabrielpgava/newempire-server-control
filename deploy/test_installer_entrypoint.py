import pathlib
import subprocess
import tempfile
import unittest

ROOT = pathlib.Path(__file__).resolve().parent.parent


class EntrypointTests(unittest.TestCase):
    def run_installer(self, *, status="200", failed=False, tamper=False, root=True):
        self.assertTrue((ROOT / "install.sh").is_file(), "pipe-compatible root install.sh is missing")
        with tempfile.TemporaryDirectory() as temporary:
            environment = {"PATH": "/usr/bin:/bin", "HOME": temporary, "TMPDIR": temporary,
                           "FIXTURE": str(ROOT / "deploy/bootstrap.py"), "TRACE": str(pathlib.Path(temporary) / "trace"),
                           "STATUS": status, "FAILED": str(int(failed)), "TAMPER": str(int(tamper)), "ROOT_USER": str(int(root))}
            functions = r'''
id() { if [[ "$ROOT_USER" == 1 ]]; then echo 0; else echo 1000; fi; }
uname() { echo Linux; }
mktemp() { /usr/bin/mktemp -d "$TMPDIR/work.XXXXXXXX"; }
timeout() { echo 93.184.216.34; }
curl() {
  local destination
  while [[ $# -gt 0 ]]; do
    if [[ "$1" == -o || "$1" == --output ]]; then destination="$2"; shift; fi
    shift
  done
  cp "$FIXTURE" "$destination"
  if [[ "$TAMPER" == 1 ]]; then echo tampered >> "$destination"; fi
  if [[ "$FAILED" == 1 ]]; then return 22; fi
  printf '%s' "$STATUS"
}
python3() { printf 'executed:%s\n' "$*" >> "$TRACE"; }
sudo() {
  [[ "$1" == /usr/bin/env && "$2" == -i && "$4" == /bin/bash && "$5" == -c ]] || return 89
  printf 'sudo\n' >> "$TRACE"
  local body="${6}"
  shift 7
  eval "$body"
}
'''
            # source /dev/stdin models a pipe without giving the installer a reusable filename.
            result = subprocess.run(["/bin/bash", "-c", functions + '\nsource /dev/stdin --version 1.2.3\n'],
                                    input=(ROOT / "install.sh").read_text(), text=True, env=environment,
                                    capture_output=True, timeout=5)
            trace = pathlib.Path(environment["TRACE"])
            return result, trace.read_text() if trace.exists() else "", list(pathlib.Path(temporary).glob("work.*"))

    def test_pipe_executes_complete_verified_bootstrap_and_cleans_up(self):
        result, trace, remaining = self.run_installer()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("executed:-I ", trace)
        self.assertIn("--version 1.2.3", trace)
        self.assertEqual(remaining, [])

    def test_download_failure_redirect_and_tampering_never_execute(self):
        for kwargs in ({"failed": True}, {"status": "302"}, {"tamper": True}):
            with self.subTest(kwargs=kwargs):
                result, trace, remaining = self.run_installer(**kwargs)
                self.assertNotEqual(result.returncode, 0)
                self.assertNotIn("executed:", trace)
                self.assertEqual(remaining, [])

    def test_nonroot_requests_sudo_with_empty_environment_and_preserves_arguments(self):
        result, trace, remaining = self.run_installer(root=False)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(trace.startswith("sudo\n"), trace)
        self.assertIn("--version 1.2.3", trace)
        self.assertEqual(remaining, [])


if __name__ == "__main__":
    unittest.main()
