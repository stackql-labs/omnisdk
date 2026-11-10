"""Offline checks of executable image defaults and explicit pinning."""
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


class PrepareInputsTest(unittest.TestCase):
    def prepare(self, *extra):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            public = root / 'existing.pub'
            public.write_text('ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFixture offline-test\n')
            case = root / 'case'
            result = subprocess.run([
                sys.executable, str(Path(__file__).with_name('prepare.py')),
                '--case', str(case), '--zone-a', 'ap-southeast-2a', '--zone-b', 'ap-southeast-2b',
                '--ssh-cidr', '203.0.113.7/32', '--public-key', str(public), *extra,
            ], capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            return json.loads((case / 'inputs.json').read_text())

    def test_default_is_regional_ssm_reference(self):
        self.assertEqual(self.prepare()['image_id'],
                         'resolve:ssm:/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64')

    def test_explicit_image_can_be_pinned(self):
        image = 'ami-00000000000000001'
        self.assertEqual(self.prepare('--image-id', image)['image_id'], image)


if __name__ == '__main__':
    unittest.main()
