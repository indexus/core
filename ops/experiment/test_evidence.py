from pathlib import Path
import tempfile
import unittest
import evidence

class EvidenceTests(unittest.TestCase):
    def test_incremental_publication_keeps_complete_lines_and_offsets(self):
        with tempfile.TemporaryDirectory() as d:
            path = Path(d)/'metrics.jsonl'
            path.write_bytes(b'first\npartial')
            sent=[]
            evidence.publish(d,'unused',lambda k,data:sent.append((k,data)))
            self.assertEqual(sent,[('metrics.jsonl/0000000000000000.part',b'first\n')])
            with path.open('ab') as f: f.write(b' end\n')
            evidence.publish(d,'unused',lambda k,data:sent.append((k,data)))
            self.assertEqual(sent[-1],('metrics.jsonl/0000000000000006.part',b'partial end\n'))
            evidence.publish(d,'unused',lambda k,data:self.fail('republished old bytes'))

    def test_upload_failure_is_retried_at_same_offset(self):
        with tempfile.TemporaryDirectory() as d:
            (Path(d)/'load.jsonl').write_text('record\n')
            def fail(k,data): raise RuntimeError('offline')
            with self.assertRaises(RuntimeError): evidence.publish(d,'unused',fail)
            sent=[]
            evidence.publish(d,'unused',lambda k,data:sent.append((k,data)))
            self.assertEqual(sent,[('load.jsonl/0000000000000000.part',b'record\n')])

if __name__ == '__main__': unittest.main()
