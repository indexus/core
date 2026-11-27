import importlib.util,json,math,tempfile,unittest
from pathlib import Path
spec=importlib.util.spec_from_file_location('builder',Path(__file__).with_name('build-weather-scene.py'))
builder=importlib.util.module_from_spec(spec);spec.loader.exec_module(builder)

class FranceTemperatureTest(unittest.TestCase):
    def test_real_zone_sums_keep_native_window_without_overlap(self):
        collection='TempeAROME0128v2'
        fine,far='4dCT','FHZL'
        lo,hi=builder.bounds(fine,collection)
        lat,lon=[(a+b)/2 for a,b in zip(lo,hi)]
        rows=[]
        for i,prefix in enumerate([fine,fine,far,far]):
            area=i+1
            rows.append({'Collection':collection,'Location':prefix+str(i)*12,
                'ID':str(i),'Metrics':[area]+[area*(270+i+h/10) for h in range(72)]})
        with tempfile.TemporaryDirectory() as tmp:
            src=Path(tmp)/(collection+'.ndjson');dest=Path(tmp)/'out.ndjson'
            src.write_text(''.join(json.dumps(row)+'\n' for row in rows))
            proof=builder.france_temperature(src,dest,lat,lon,100,12)
            out=[json.loads(line) for line in dest.read_text().splitlines()]
        self.assertEqual(proof['native_records'],2)
        self.assertEqual(proof['coarse_records'],1)
        self.assertEqual(out[:2],rows[:2])
        self.assertEqual(out[-1]['Location'],far)
        self.assertEqual(len({row['ID'] for row in out}),3)
        for i in range(73):
            self.assertTrue(math.isclose(sum(r['Metrics'][i] for r in rows),sum(r['Metrics'][i] for r in out),rel_tol=1e-12))
        self.assertFalse(any(a['Location']!=b['Location'] and a['Location'].startswith(b['Location']) for a in out for b in out))

if __name__=='__main__':unittest.main()
