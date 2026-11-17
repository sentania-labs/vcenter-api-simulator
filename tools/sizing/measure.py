#!/usr/bin/env python3
"""Linux RSS/CPU and health measurement with two independent full property clients.
Usage: measure.py small|medium|large /absolute/vcsimulator run-name /scratch/path
Run under systemd-run --user --scope -p MemoryMax=6G [-p CPUQuota=100%].
Full property reads are a conservative upper envelope, not an Operations replay.
Uses free loopback ports, a private account, full query capture and PID cleanup.
"""
import os,sys,time,json,subprocess,threading,ssl,urllib.request,xml.etree.ElementTree as ET,concurrent.futures,socket
from pathlib import Path
root=Path(__file__).resolve().parents[2]
scratch=Path(sys.argv[4]).resolve(); scratch.mkdir(parents=True,exist_ok=True); (scratch/'tmp').mkdir(exist_ok=True)
sys.dont_write_bytecode=True
sys.path.insert(0,str(root/'tools/bench')); import bench
size=sys.argv[1]; binary=sys.argv[2]; run=sys.argv[3]
n=7 if size=='large' else 4 if size=='medium' else 1
hosts=215 if n==7 else 32 if n==4 else 4
cfg='seed: sizing\nvcenterVersion: {preset: 9.1.1}\nesxVersion: {preset: 9.1.1}\ndatacenters:\n  - name: dc-01\n    clusters: '+str(n)+'\ndefaults:\n  hosts: '+str(4 if n==1 else 8)+'\n  host: {claimedDevices: '+str(1 if n==1 else 12)+', localDevices: 1}\n  vms: {perHost: '+str(1 if n==1 else 25)+', disksPerVM: 1}\n'
if n==7:
 cfg+='clusters:\n'+''.join(f'  - key: dc-01/cluster-0{i}\n    hosts: 61\n    vms: {{perHost: 23, disksPerVM: 1}}\n' for i in (5,6,7))
(scratch/'scenario.yaml').write_text(cfg)
data=scratch/run; data.mkdir(exist_ok=True)
def freeport():
 with socket.socket() as sock:
  sock.bind(('127.0.0.1',0)); return sock.getsockname()[1]
port=freeport(); adminport=freeport()
ctx=ssl._create_unverified_context(); base=f'https://127.0.0.1:{port}'
log=open(scratch/(run+'.log'),'w'); start=time.monotonic()
p=subprocess.Popen([binary,'-scenario',str(scratch/'scenario.yaml'),'-data',str(data),'-listen',f'127.0.0.1:{port}','-admin',f'127.0.0.1:{adminport}','-username','sizing','-password','sizing','-query-log',str(data/'query-log.jsonl')],stdout=log,stderr=log,env={**os.environ,'TMPDIR':str(scratch/'tmp')})
peak=0; stop=False; probes=[]; cpu=0.0; failures=0
def monitor():
 global peak, cpu
 while not stop:
  try:
   vals=dict(l.split(':',1) for l in Path(f'/proc/{p.pid}/status').read_text().splitlines() if ':' in l)
   peak=max(peak,int(vals['VmRSS'].split()[0]))
   fields=Path(f'/proc/{p.pid}/stat').read_text().split(); cpu=(int(fields[13])+int(fields[14]))/os.sysconf('SC_CLK_TCK')
  except (OSError,KeyError): pass
  time.sleep(.02)
def probe():
 global failures
 while not stop:
  t=time.monotonic()
  try:
   with urllib.request.urlopen(base+'/sdk/vimServiceVersions.xml',context=ctx,timeout=5) as r:r.read()
   probes.append(time.monotonic()-t)
  except Exception:failures+=1
  time.sleep(.1)
threading.Thread(target=monitor,daemon=True).start(); threading.Thread(target=probe,daemon=True).start()
try:
 while not probes:
  if p.poll() is not None:raise RuntimeError('server exited '+str(p.returncode))
  if time.monotonic()-start>180:raise RuntimeError('startup timeout')
  time.sleep(.1)
 first=time.monotonic()-start
 while 'vcsimulator: listening on' not in (scratch/(run+'.log')).read_text():
  if time.monotonic()-start>180:raise RuntimeError('inventory build timeout')
  if p.poll() is not None:raise RuntimeError('own simulator exited')
  time.sleep(.1)
 build=time.monotonic()-start; buildcpu=cpu
 time.sleep(2)
 rss=int(next(l for l in Path(f'/proc/{p.pid}/status').read_text().splitlines() if l.startswith('VmRSS:')).split()[1])
 def collect(_):
  jar=__import__('http.cookiejar',fromlist=['CookieJar']).CookieJar()
  op=urllib.request.build_opener(urllib.request.HTTPSHandler(context=ctx),urllib.request.HTTPCookieProcessor(jar))
  def post(body):
   with op.open(urllib.request.Request(base+'/sdk',body.encode(),{'Content-Type':'text/xml','SOAPAction':'"urn:vim25/8.0.2.0"'}),timeout=120) as r: return r.read()
  content=ET.fromstring(post(bench.sdk_service_content()))
  refs={e.tag.split('}')[-1]:e.text for e in content.iter() if e.get('type')}
  login=bench.sdk_login('sizing','sizing').replace('>SessionManager<','>'+refs['sessionManager']+'<')
  post(login)
  counts={}
  for typ in ['Datacenter','ClusterComputeResource','HostSystem','Datastore','ResourcePool','VirtualMachine','DistributedVirtualSwitch','DistributedVirtualPortgroup']:
   view=ET.fromstring(post(bench.sdk_container_view(typ).replace('>ViewManager<','>'+refs['viewManager']+'<').replace('>group-d1<','>'+refs['rootFolder']+'<')))
   ref=next(e.text for e in view.iter() if e.tag.endswith('returnval'))
   spec=f'<specSet><propSet><type>{typ}</type><all>true</all></propSet><objectSet><obj type="ContainerView">{ref}</obj><skip>true</skip><selectSet xsi:type="TraversalSpec"><name>view</name><type>ContainerView</type><path>view</path><skip>false</skip></selectSet></objectSet></specSet><options><maxObjects>250</maxObjects></options>'
   out=post(bench.body('RetrievePropertiesEx','PropertyCollector',refs['propertyCollector'],spec)); counts[typ]=0
   while True:
    tree=ET.fromstring(out); counts[typ]+=sum(e.tag.split('}')[-1]=='objects' for e in tree.iter()); tokens=[e.text for e in tree.iter() if e.tag.endswith('token')]
    if not tokens:break
    out=post(bench.body('ContinueRetrievePropertiesEx','PropertyCollector',refs['propertyCollector'],'<token>'+tokens[0]+'</token>'))
  assert counts['HostSystem']==hosts and counts['VirtualMachine']==(5009 if n==7 else 800 if n==4 else 4), counts
  post(bench.sdk_logout().replace('>SessionManager<','>'+refs['sessionManager']+'<'));return counts
 load=time.monotonic()
 with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:results=list(pool.map(collect,range(2)))
 elapsed=time.monotonic()-load
 print(json.dumps(dict(size=size,run=run,hosts=hosts,first_s=first,rss_mib=rss/1024,peak_mib=peak/1024,collect_s=elapsed,health_max_s=max(probes),health_failures=failures,build_s=build,build_cpu_s=buildcpu,total_cpu_s=cpu,responses=results)),flush=True)
finally:
 stop=True;p.terminate()
 try:p.wait(timeout=10)
 except subprocess.TimeoutExpired:p.kill();p.wait()
 log.close()
