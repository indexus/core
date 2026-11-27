import unittest
import loader_tail


class LoaderTailTests(unittest.TestCase):
    def test_identical_minimal_instrumentation_is_idempotent(self):
        original = 'summary := map[string]any{'+loader_tail.ANCHOR+'}'
        patched = loader_tail.instrument(original)
        self.assertEqual(loader_tail.instrument(patched), patched)
        for key, variable in loader_tail.FIELDS.items():
            self.assertIn('"'+key+'": '+variable+'.drain()', patched)
        self.assertEqual(patched.count('drain_completed'), 1)

    def test_unknown_loader_is_rejected_instead_of_partial_patch(self):
        with self.assertRaises(ValueError):
            loader_tail.instrument('different loader')
        with self.assertRaises(ValueError):
            loader_tail.instrument(loader_tail.ANCHOR+' '+loader_tail.ANCHOR)

    def test_reviewed_old_wait_layout_gets_counts_and_remainders(self):
        source = '\tclose(jobs)\n\twg.Wait()\n\tfmt.Fprintf(os.Stderr, "offered=%d")'
        patched = loader_tail.instrument(source)
        self.assertIn('"drain_completed": true', patched)
        self.assertIn('"exhausted": offered.Load()-shed.Load()-ackedN.Load()', patched)
        self.assertEqual(loader_tail.instrument(patched), patched)
