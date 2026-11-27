import importlib.util,json,math,unittest
from pathlib import Path
spec=importlib.util.spec_from_file_location('builder',Path(__file__).with_name('build-dvf-scene.py'))
builder=importlib.util.module_from_spec(spec);spec.loader.exec_module(builder)
class LocalSalesTest(unittest.TestCase):
    def row(self,x,y):
        scale=math.pi*6378137/180;lat=43.484986
        return {'ID':f'{x}/{y}','Metrics':[500000,100,5000,lat+y/scale+90,-1.520707+x/(scale*math.cos(math.radians(lat)))+180]}
    def test_radius_expansion_retains_records_without_reencoding(self):
        row=self.row(300,300)
        self.assertEqual(list(builder.local_sales([json.dumps(row)],radius=150)),[])
        self.assertEqual(list(builder.local_sales([json.dumps(row)],radius=500)),[row])
    def test_disc_excludes_bounding_square_corners_and_deleted_sales(self):
        outside=self.row(400,400);deleted=self.row(0,0);deleted['Deleted']=True
        self.assertEqual(list(builder.local_sales(map(json.dumps,[outside,deleted]),radius=500)),[])
if __name__=='__main__':unittest.main()
